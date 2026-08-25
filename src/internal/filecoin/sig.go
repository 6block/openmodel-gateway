// Package filecoin provides the minimal native-chain access the gateway needs for
// SP self-registration: reading miner state over public Lotus JSON-RPC endpoints
// and verifying wallet signatures (secp256k1 and BLS) locally — byte-compatible
// with what `Filecoin.WalletSign` produces, without trusting a remote
// WalletVerify.
package filecoin

import (
	"bytes"
	"fmt"

	bls12381 "github.com/drand/kyber-bls12381"
	"github.com/drand/kyber/sign/bls"
	"github.com/ethereum/go-ethereum/crypto"
	faddr "github.com/filecoin-project/go-address"
	"golang.org/x/crypto/blake2b"
)

// Signature type identifiers as used by Lotus (crypto.SigType) and returned in the
// JSON form of Filecoin.WalletSign: {"Type": N, "Data": base64}.
const (
	SigTypeSecp256k1 uint64 = 1
	SigTypeBLS       uint64 = 2
)

// VerifySignature reports whether sig (of sigType) over msg was produced by the key
// behind signer — an f1/t1 (secp256k1) or f3/t3 (BLS) key address. The t/f network
// prefix is ignored: only protocol and payload are compared, so a calibnet address
// string verifies the same as its mainnet form.
func VerifySignature(sigType uint64, sig []byte, signer string, msg []byte) error {
	addr, err := faddr.NewFromString(signer)
	if err != nil {
		return fmt.Errorf("bad signer address %q: %w", signer, err)
	}
	switch sigType {
	case SigTypeSecp256k1:
		return verifySecp(sig, addr, msg)
	case SigTypeBLS:
		return verifyBLS(sig, addr, msg)
	default:
		return fmt.Errorf("unsupported signature type %d (want 1=secp256k1 or 2=bls)", sigType)
	}
}

// verifySecp checks a Lotus secp256k1 signature: 65 bytes [R|S|V] over
// blake2b-256(msg), where the recovered public key must hash (blake2b-160) to the
// signer address payload. Lotus emits V ∈ {0,1}; the Ethereum-style +27 offset is
// normalized in case the signature came through eth tooling.
func verifySecp(sig []byte, addr faddr.Address, msg []byte) error {
	if addr.Protocol() != faddr.SECP256K1 {
		return fmt.Errorf("signature type is secp256k1 but signer %s is not an f1 address", addr)
	}
	if len(sig) != 65 {
		return fmt.Errorf("secp256k1 signature must be 65 bytes, got %d", len(sig))
	}
	norm := make([]byte, 65)
	copy(norm, sig)
	if norm[64] >= 27 {
		norm[64] -= 27
	}
	if norm[64] != 0 && norm[64] != 1 {
		return fmt.Errorf("invalid recovery id %d", sig[64])
	}
	digest := blake2b.Sum256(msg)
	pub, err := crypto.Ecrecover(digest[:], norm)
	if err != nil {
		return fmt.Errorf("pubkey recovery failed: %w", err)
	}
	payload := blake2b160(pub)
	if !bytes.Equal(payload, addr.Payload()) {
		return fmt.Errorf("signature does not match signer address %s", addr)
	}
	return nil
}

var (
	blsSuite  = bls12381.NewBLS12381Suite()
	blsScheme = bls.NewSchemeOnG2(blsSuite)
)

// verifyBLS checks a Filecoin BLS signature (min-pk: 48-byte G1 public key,
// 96-byte G2 signature, ciphersuite BLS_SIG_BLS12381G2_XMD:SHA-256_SSWU_RO_NUL_).
// An f3 address payload IS the compressed public key, so no chain lookup is needed.
func verifyBLS(sig []byte, addr faddr.Address, msg []byte) error {
	if addr.Protocol() != faddr.BLS {
		return fmt.Errorf("signature type is bls but signer %s is not an f3 address", addr)
	}
	if len(sig) != 96 {
		return fmt.Errorf("bls signature must be 96 bytes, got %d", len(sig))
	}
	pub := blsSuite.G1().Point()
	if err := pub.UnmarshalBinary(addr.Payload()); err != nil {
		return fmt.Errorf("bad bls pubkey in signer address: %w", err)
	}
	if err := blsScheme.Verify(pub, msg, sig); err != nil {
		return fmt.Errorf("bls signature does not match signer address %s", addr)
	}
	return nil
}

// SameKey reports whether two address strings refer to the same key, ignoring the
// t/f network prefix (protocol + payload comparison).
func SameKey(a, b string) bool {
	pa, err1 := faddr.NewFromString(a)
	pb, err2 := faddr.NewFromString(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return pa.Protocol() == pb.Protocol() && bytes.Equal(pa.Payload(), pb.Payload())
}

func blake2b160(data []byte) []byte {
	h, err := blake2b.New(20, nil)
	if err != nil {
		panic(err) // static parameters; cannot fail
	}
	h.Write(data)
	return h.Sum(nil)
}
