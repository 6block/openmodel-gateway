package gateway

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"openmodel/sp-state-agent/internal/filecoin"
	"openmodel/sp-state-agent/internal/worker"
)

// spregister.go implements SP (worker) SELF-registration on the public port —
// the M1 stack calls it, no operator involved:
//
//	① POST /v1/sp/challenge {miner_id}                → one-time server challenge
//	② SP signs the registration message with the miner's OWNER or WORKER key
//	   (via its local Lotus `Filecoin.WalletSign`)
//	③ POST /v1/sp/register {…, signer, signature}     → admission checks → auth token
//
// Admission = proof of miner-key control (signature) + storage-scale floor (raw
// byte power) + mining-stake floor (miner actor balance). A miner that passes is
// registered as a routable worker whose earnings settle to the miner-SIGNED payout
// address. Punishments (routing ban + frozen-earnings confiscation) are handled
// elsewhere; re-registration deliberately does NOT clear an active ban.

// ChainReader is the miner-state access spRegistrar needs (implemented by
// filecoin.Client; faked in tests).
type ChainReader interface {
	MinerInfo(ctx context.Context, miner string) (ownerID, workerID string, err error)
	AccountKey(ctx context.Context, idAddr string) (string, error)
	MinerRawPower(ctx context.Context, miner string) (*big.Int, error)
	ActorBalance(ctx context.Context, addr string) (*big.Int, error)
}

// SPRegistrationOptions mirrors config.SPRegistrationConfig with parsed values
// (kept separate so the gateway package does not depend on the config package).
type SPRegistrationOptions struct {
	GatewayID          string
	MinRawPowerBytes   *big.Int // nil/zero = check disabled
	MinMinerBalance    *big.Int // attoFIL; nil/zero = check disabled
	ChallengeTTL       time.Duration
	RegisterRatePerMin int
	MaxRegisteredSPs   int
}

type spChallenge struct {
	miner   string
	expires time.Time
}

type spRegistrar struct {
	opts       SPRegistrationOptions
	chain      ChainReader
	mu         sync.Mutex // guards challenges AND the check-then-register sequence
	challenges map[string]spChallenge
	ipl        *ipLimiter
	logger     *slog.Logger
}

// EnableSPRegistration turns on SP self-registration. Must be called before
// Handler(). The routes exist regardless; without this call they answer 404.
func (g *Gateway) EnableSPRegistration(opts SPRegistrationOptions, chain ChainReader) {
	if opts.ChallengeTTL <= 0 {
		opts.ChallengeTTL = 10 * time.Minute
	}
	if opts.RegisterRatePerMin <= 0 {
		opts.RegisterRatePerMin = 6
	}
	if opts.MaxRegisteredSPs <= 0 {
		opts.MaxRegisteredSPs = 1000
	}
	g.spReg = &spRegistrar{
		opts:       opts,
		chain:      chain,
		challenges: make(map[string]spChallenge),
		ipl:        newIPLimiter(float64(opts.RegisterRatePerMin)),
		logger:     g.logger,
	}
}

var minerIDRe = regexp.MustCompile(`^[tf]0[0-9]+$`)

// Must match (be a subset of) worker.validWorkerID — a self-registered id the
// registry would refuse to store must be rejected at the door, not after.
var spWorkerIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// buildSPRegistrationMessage is the EXACT text the SP signs. The M1 registration
// client (go-scheduler internal/spregister) constructs the identical message from
// its own field values — any change here must stay in lockstep with it and with
// the documented signing format.
func buildSPRegistrationMessage(minerID, gatewayID, workerID, payout, endpoint, schedulerURL, challenge string) string {
	return "OpenModel SP registration\n" +
		"miner: " + minerID + "\n" +
		"gateway: " + gatewayID + "\n" +
		"worker_id: " + workerID + "\n" +
		"payout: " + payout + "\n" +
		"endpoint: " + endpoint + "\n" +
		"scheduler_url: " + schedulerURL + "\n" +
		"challenge: " + challenge
}

type spChallengeRequest struct {
	MinerID string `json:"miner_id"`
}

type spChallengeResponse struct {
	Challenge string `json:"challenge"`
	ExpiresAt int64  `json:"expires_at"`
}

type spSignature struct {
	Type uint64 `json:"type"` // 1 = secp256k1, 2 = bls (Lotus WalletSign JSON form)
	Data string `json:"data"` // base64
}

type spRegisterRequest struct {
	MinerID       string `json:"miner_id"`
	WorkerID      string `json:"worker_id"`
	PayoutAddress string `json:"payout_address"`
	Endpoint      string `json:"endpoint"`
	SchedulerURL  string `json:"scheduler_url"`
	// SupportedModels: what this worker claims it can serve (loaded + switchable).
	// Claims, not facts — with the admission gate on, each entry still has to
	// pass its first probe verification before any traffic routes to it.
	SupportedModels []string    `json:"supported_models,omitempty"`
	GPUCount        int         `json:"gpu_count,omitempty"`
	Challenge       string      `json:"challenge"`
	Signer          string      `json:"signer"` // f1/f3 key address that produced the signature
	Signature       spSignature `json:"signature"`
	// CSR (PEM, optional): when the gateway has an issuing CA configured, the
	// worker's certificate request is signed as part of registration — same
	// admission, same response, no out-of-band step. Only the public key and a
	// matching CN are used; everything else in the CSR is ignored.
	CSR string `json:"csr,omitempty"`
}

type spRegisterResponse struct {
	WorkerID      string `json:"worker_id"`
	AuthToken     string `json:"auth_token"`
	MinerID       string `json:"miner_id"`
	PayoutAddress string `json:"payout_address"`
	State         string `json:"state"`   // "registered"
	Rotated       bool   `json:"rotated"` // true when an existing registration was updated (token rotated)
	// Set when a CSR was submitted and the gateway has an issuing CA: the
	// worker's short-lived server certificate and the CA cert its TLS front
	// needs to verify the gateway's client certificate.
	WorkerCert       string `json:"worker_cert,omitempty"`
	CACert           string `json:"ca_cert,omitempty"`
	CertNotAfterUnix int64  `json:"cert_not_after_unix,omitempty"`
}

// handleSPChallenge: POST /v1/sp/challenge {miner_id} → {challenge, expires_at}.
func (g *Gateway) handleSPChallenge(w http.ResponseWriter, r *http.Request) {
	reg := g.spReg
	if reg == nil {
		jsonError(w, "sp self-registration is not enabled on this gateway", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed; use POST", http.StatusMethodNotAllowed)
		return
	}
	if !reg.ipl.allow(clientIP(r)) {
		jsonError(w, "too many registration attempts; slow down", http.StatusTooManyRequests)
		return
	}
	var req spChallengeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if !minerIDRe.MatchString(req.MinerID) {
		jsonError(w, "miner_id must be an ID address like t01234 / f01234", http.StatusBadRequest)
		return
	}

	buf := make([]byte, 32)
	if _, err := cryptorand.Read(buf); err != nil {
		jsonError(w, "failed to generate challenge", http.StatusInternalServerError)
		return
	}
	challenge := hex.EncodeToString(buf)
	expires := time.Now().Add(reg.opts.ChallengeTTL)

	reg.mu.Lock()
	reg.pruneChallengesLocked()
	reg.challenges[challenge] = spChallenge{miner: req.MinerID, expires: expires}
	reg.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(spChallengeResponse{Challenge: challenge, ExpiresAt: expires.Unix()})
}

// handleSPRegister: POST /v1/sp/register — the full admission pipeline.
func (g *Gateway) handleSPRegister(w http.ResponseWriter, r *http.Request) {
	reg := g.spReg
	if reg == nil {
		jsonError(w, "sp self-registration is not enabled on this gateway", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed; use POST", http.StatusMethodNotAllowed)
		return
	}
	if !reg.ipl.allow(clientIP(r)) {
		jsonError(w, "too many registration attempts; slow down", http.StatusTooManyRequests)
		return
	}
	var req spRegisterRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// --- field validation (cheap, before any chain reads) ---
	if !minerIDRe.MatchString(req.MinerID) {
		jsonError(w, "miner_id must be an ID address like t01234 / f01234", http.StatusBadRequest)
		return
	}
	if !spWorkerIDRe.MatchString(req.WorkerID) {
		jsonError(w, "worker_id must be 1-64 alphanumeric characters, hyphens, underscores, or dots", http.StatusBadRequest)
		return
	}
	if !common.IsHexAddress(req.PayoutAddress) {
		jsonError(w, "payout_address must be a 0x EVM address", http.StatusBadRequest)
		return
	}
	if !validHTTPURL(req.Endpoint) || !validHTTPURL(req.SchedulerURL) {
		jsonError(w, "endpoint and scheduler_url must be valid http(s) URLs", http.StatusBadRequest)
		return
	}
	sigBytes, err := base64.StdEncoding.DecodeString(req.Signature.Data)
	if err != nil || len(sigBytes) == 0 {
		jsonError(w, "signature.data must be base64 (Lotus WalletSign form)", http.StatusBadRequest)
		return
	}

	// --- challenge: must exist, be fresh, match the miner, and is consumed on use
	// (a failed attempt burns it; ask for a new one) ---
	reg.mu.Lock()
	ch, ok := reg.challenges[req.Challenge]
	if ok {
		delete(reg.challenges, req.Challenge)
	}
	reg.mu.Unlock()
	if !ok || time.Now().After(ch.expires) || ch.miner != req.MinerID {
		jsonError(w, "challenge invalid, expired, or for a different miner; request a new one via /v1/sp/challenge", http.StatusBadRequest)
		return
	}

	// --- signature: reconstructed message must verify against the claimed signer ---
	msg := buildSPRegistrationMessage(req.MinerID, reg.opts.GatewayID, req.WorkerID,
		req.PayoutAddress, req.Endpoint, req.SchedulerURL, req.Challenge)
	if err := filecoin.VerifySignature(req.Signature.Type, sigBytes, req.Signer, []byte(msg)); err != nil {
		jsonError(w, fmt.Sprintf("signature verification failed: %v", err), http.StatusUnauthorized)
		return
	}

	// --- signer must be the miner's worker or owner key (proves miner control) ---
	ctx := r.Context()
	ownerID, workerKeyID, err := reg.chain.MinerInfo(ctx, req.MinerID)
	if err != nil {
		jsonError(w, fmt.Sprintf("cannot resolve miner %s on chain: %v", req.MinerID, err), http.StatusBadGateway)
		return
	}
	matched, err := reg.signerControlsMiner(ctx, req.Signer, ownerID, workerKeyID)
	if err != nil {
		jsonError(w, fmt.Sprintf("cannot resolve miner keys: %v", err), http.StatusBadGateway)
		return
	}
	if !matched {
		jsonError(w, fmt.Sprintf("signer %s is neither the worker nor the owner key of miner %s", req.Signer, req.MinerID), http.StatusForbidden)
		return
	}

	// --- admission thresholds ---
	if min := reg.opts.MinRawPowerBytes; min != nil && min.Sign() > 0 {
		power, err := reg.chain.MinerRawPower(ctx, req.MinerID)
		if err != nil {
			jsonError(w, fmt.Sprintf("cannot read miner power: %v", err), http.StatusBadGateway)
			return
		}
		if power.Cmp(min) < 0 {
			jsonError(w, fmt.Sprintf("miner raw byte power %s below the admission floor %s", power, min), http.StatusForbidden)
			return
		}
	}
	if min := reg.opts.MinMinerBalance; min != nil && min.Sign() > 0 {
		bal, err := reg.chain.ActorBalance(ctx, req.MinerID)
		if err != nil {
			jsonError(w, fmt.Sprintf("cannot read miner balance: %v", err), http.StatusBadGateway)
			return
		}
		if bal.Cmp(min) < 0 {
			jsonError(w, fmt.Sprintf("miner balance %s attoFIL below the admission floor %s attoFIL", bal, min), http.StatusForbidden)
			return
		}
	}

	payout := common.HexToAddress(req.PayoutAddress).Hex()

	// --- uniqueness + insert, serialized against concurrent registrations ---
	reg.mu.Lock()
	defer reg.mu.Unlock()

	rotated := false
	if existing, found := g.registry.FindByMiner(req.MinerID); found {
		if existing.ID != req.WorkerID {
			jsonError(w, fmt.Sprintf("miner %s is already registered as worker %q; deregister it first or reuse that worker_id", req.MinerID, existing.ID), http.StatusConflict)
			return
		}
		rotated = true // same miner re-proving key control: update + token rotation
	}
	if existing, found := g.registry.Get(req.WorkerID); found && existing.MinerAddress != req.MinerID {
		jsonError(w, fmt.Sprintf("worker_id %q is already in use", req.WorkerID), http.StatusConflict)
		return
	}
	if !rotated {
		if _, found := g.registry.Get(req.WorkerID); !found && g.registry.Count() >= reg.opts.MaxRegisteredSPs {
			jsonError(w, "registration capacity reached", http.StatusServiceUnavailable)
			return
		}
	}

	token, err := generateWorkerToken()
	if err != nil {
		jsonError(w, "failed to generate auth token", http.StatusInternalServerError)
		return
	}
	// Bound and sanitize the claimed model list: it feeds routing and the
	// admission auditor's first-verification queue, so an unbounded or junk list
	// would let one registration flood both.
	models := req.SupportedModels
	if len(models) > 16 {
		models = models[:16]
	}
	cleaned := models[:0]
	for _, m := range models {
		if l := len(strings.TrimSpace(m)); l > 0 && l <= 128 {
			cleaned = append(cleaned, strings.TrimSpace(m))
		}
	}
	if _, err := g.registry.Register(worker.WorkerRegistration{
		ID:              req.WorkerID,
		SupportedModels: cleaned,
		Endpoint:        req.Endpoint,
		SchedulerURL:    req.SchedulerURL,
		GPUCount:        req.GPUCount,
		MinerAddress:    req.MinerID,
		AuthToken:       token,
		PayoutAddress:   payout,
		SelfRegistered:  true,
	}); err != nil {
		jsonError(w, fmt.Sprintf("registration rejected: %v", err), http.StatusBadRequest)
		return
	}

	// Audit line: every admission decision must be reconstructible.
	g.logger.Info("sp self-registered",
		"worker_id", req.WorkerID,
		"miner", req.MinerID,
		"payout", payout,
		"signer", req.Signer,
		"endpoint", req.Endpoint,
		"rotated", rotated,
	)

	resp := spRegisterResponse{
		WorkerID:      req.WorkerID,
		AuthToken:     token,
		MinerID:       req.MinerID,
		PayoutAddress: payout,
		State:         "registered",
		Rotated:       rotated,
	}
	// Certificate-at-registration: admission just passed, so this CSR is signed
	// here and now. Issue failures are reported inside the response rather than
	// failing the registration — the worker is REGISTERED (plaintext works); a
	// bad CSR only costs it the certificate, and the error text says why.
	if g.certIssuer != nil && req.CSR != "" {
		certPEM, cerr := g.certIssuer.issueFromCSR(req.CSR, req.WorkerID)
		if cerr != nil {
			g.logger.Warn("worker CSR rejected", "worker_id", req.WorkerID, "error", cerr)
		} else {
			resp.WorkerCert = certPEM
			resp.CACert = g.certIssuer.caCertPEM()
			resp.CertNotAfterUnix = time.Now().Add(g.certIssuer.validity).Unix()
			g.logger.Info("worker certificate issued at registration",
				"worker_id", req.WorkerID, "validity", g.certIssuer.validity.String())
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleSPRenewCert: POST /v1/sp/renew-cert {csr} with the worker's Bearer
// token. Short-lived certificates make renewal the ONLY revocation mechanism:
// a banned or deregistered worker is refused here and simply ages out of the
// mTLS layer within one validity window — the routing ban quietly escalates to
// network-layer removal with no CRL machinery.
func (g *Gateway) handleSPRenewCert(w http.ResponseWriter, r *http.Request) {
	if g.certIssuer == nil {
		jsonError(w, "certificate issuing is not enabled on this gateway", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed; use POST", http.StatusMethodNotAllowed)
		return
	}
	token := bearerToken(r)
	if token == "" {
		jsonError(w, "missing worker Bearer token", http.StatusUnauthorized)
		return
	}
	wk, ok := g.registry.FindByToken(token)
	if !ok {
		jsonError(w, "unknown worker token (deregistered? re-register first)", http.StatusUnauthorized)
		return
	}
	if wk.IsBanned() {
		jsonError(w, "worker is banned from routing; certificate renewal refused", http.StatusForbidden)
		return
	}
	var req struct {
		CSR string `json:"csr"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil || req.CSR == "" {
		jsonError(w, "body must be {\"csr\": \"<PEM>\"}", http.StatusBadRequest)
		return
	}
	certPEM, err := g.certIssuer.issueFromCSR(req.CSR, wk.ID)
	if err != nil {
		jsonError(w, "csr rejected: "+err.Error(), http.StatusBadRequest)
		return
	}
	g.logger.Info("worker certificate renewed", "worker_id", wk.ID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"worker_id":           wk.ID,
		"worker_cert":         certPEM,
		"ca_cert":             g.certIssuer.caCertPEM(),
		"cert_not_after_unix": time.Now().Add(g.certIssuer.validity).Unix(),
	})
}

// signerControlsMiner reports whether signer is the miner's worker or owner key.
// The worker key is resolved first (the common signer), the owner key only when
// needed — one chain read saved on the hot path.
func (r *spRegistrar) signerControlsMiner(ctx context.Context, signer, ownerID, workerKeyID string) (bool, error) {
	workerKey, err := r.chain.AccountKey(ctx, workerKeyID)
	if err != nil {
		return false, fmt.Errorf("worker key of %s: %w", workerKeyID, err)
	}
	if filecoin.SameKey(signer, workerKey) {
		return true, nil
	}
	ownerKey, err := r.chain.AccountKey(ctx, ownerID)
	if err != nil {
		return false, fmt.Errorf("owner key of %s: %w", ownerID, err)
	}
	return filecoin.SameKey(signer, ownerKey), nil
}

// pruneChallengesLocked drops expired challenges. Caller holds r.mu.
func (r *spRegistrar) pruneChallengesLocked() {
	now := time.Now()
	for c, ch := range r.challenges {
		if now.After(ch.expires) {
			delete(r.challenges, c)
		}
	}
}

// generateWorkerToken returns a fresh per-worker bearer secret. "wk-" marks it as
// a gateway-issued worker token (distinct from client API keys).
func generateWorkerToken() (string, error) {
	b := make([]byte, 24)
	if _, err := cryptorand.Read(b); err != nil {
		return "", err
	}
	return "wk-" + hex.EncodeToString(b), nil
}

func validHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// bearerToken extracts the Bearer credential ("" when absent).
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return ""
}

// EnableCertIssuer arms certificate-at-registration with the issuing CA. Call
// before Handler(); without it registration works but issues no certificates
// and /v1/sp/renew-cert answers 404.
func (g *Gateway) EnableCertIssuer(caCertFile, caKeyFile string, validity time.Duration) error {
	ci, err := newCertIssuer(caCertFile, caKeyFile, validity)
	if err != nil {
		return err
	}
	g.certIssuer = ci
	if ci != nil {
		g.logger.Info("certificate-at-registration enabled", "validity", ci.validity.String())
	}
	return nil
}

// StartWorkerTLS serves the gateway's routes over TLS on addr, presenting a
// server certificate signed by the issuing CA (SAN = gateway_id). This is the
// worker→gateway direction of transport security — registration, token
// issuance, renewals and the admission self-view stop being plaintext the
// moment they leave the tunnel for the public internet. Callers run it in a
// goroutine; it returns on listener failure.
func (g *Gateway) StartWorkerTLS(addr, gatewayID string, handler http.Handler) error {
	if g.certIssuer == nil {
		return fmt.Errorf("worker https: certificate issuer (issuer_ca_cert/key) is not configured")
	}
	cert, err := g.certIssuer.serverTLSCert(gatewayID)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:      addr,
		Handler:   handler,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}
	g.logger.Info("worker-direction HTTPS enabled",
		"addr", addr, "server_name", gatewayID, "issuer", "registration CA")
	return srv.ListenAndServeTLS("", "")
}

// trustedProxies holds the reverse proxies whose forwarding headers clientIP
// believes. Empty (the default) means headers are never trusted — RemoteAddr
// is the client, exactly as before the domain fronting existed.
var trustedProxies []*net.IPNet

// SetTrustedProxies installs the reverse-proxy allowlist (IPs or CIDRs) that
// per-IP rate limits see through. Without it, every request arriving via the
// domain front shares the proxy's address and all users share one bucket.
func SetTrustedProxies(entries []string) error {
	var nets []*net.IPNet
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.Contains(e, "/") {
			if ip := net.ParseIP(e); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				e = fmt.Sprintf("%s/%d", ip.String(), bits)
			}
		}
		_, ipnet, err := net.ParseCIDR(e)
		if err != nil {
			return fmt.Errorf("trusted_proxies entry %q: %w", e, err)
		}
		nets = append(nets, ipnet)
	}
	trustedProxies = nets
	return nil
}

func isTrustedProxy(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range trustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP is what every per-IP rate limit keys on. A forwarding header is
// believed ONLY when the direct peer is a listed trusted proxy — anyone else
// could fabricate it. From a trusted peer, the X-Forwarded-For chain is walked
// right to left past other trusted hops to the first external address (the
// rightmost entries were appended by our own proxies; the leftmost are
// client-supplied and stay untrusted).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !isTrustedProxy(host) {
		return host
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			p := strings.TrimSpace(parts[i])
			if p != "" && !isTrustedProxy(p) {
				return p
			}
		}
	}
	if rip := strings.TrimSpace(r.Header.Get("X-Real-IP")); rip != "" {
		return rip
	}
	return host
}

// ParseFILToAtto converts a decimal FIL string ("10.5") to attoFIL. Empty or "0"
// yields zero (= threshold disabled).
func ParseFILToAtto(s string) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return new(big.Int), nil
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 {
		return nil, fmt.Errorf("invalid FIL amount %q", s)
	}
	atto := new(big.Rat).Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)))
	return new(big.Int).Quo(atto.Num(), atto.Denom()), nil
}

// ipLimiter is a per-IP token bucket for the (pre-auth) registration endpoints —
// the per-key B5 limiter cannot cover callers that don't have a key yet.
type ipLimiter struct {
	mu      sync.Mutex
	perSec  float64
	burst   float64
	buckets map[string]*ipBucket
}

type ipBucket struct {
	tokens float64
	last   time.Time
}

func newIPLimiter(perMin float64) *ipLimiter {
	return &ipLimiter{
		perSec:  perMin / 60.0,
		burst:   perMin,
		buckets: make(map[string]*ipBucket),
	}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[ip]
	if !ok {
		if len(l.buckets) >= 4096 { // bound memory under address-spread abuse
			l.pruneLocked(now)
		}
		b = &ipBucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.perSec
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// pruneLocked evicts buckets idle long enough to have refilled completely.
func (l *ipLimiter) pruneLocked(now time.Time) {
	for ip, b := range l.buckets {
		if now.Sub(b.last).Seconds()*l.perSec >= l.burst {
			delete(l.buckets, ip)
		}
	}
}
