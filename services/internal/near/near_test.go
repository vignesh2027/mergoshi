package near

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"testing"
)

func TestBase58RoundTrip(t *testing.T) {
	cases := [][]byte{{}, {0}, {0, 0, 1}, []byte("hello world"), bytes.Repeat([]byte{255}, 32)}
	for _, c := range cases {
		enc := Base58Encode(c)
		dec, err := Base58Decode(enc)
		if err != nil || !bytes.Equal(dec, c) {
			t.Fatalf("round trip %x: got %x (%v)", c, dec, err)
		}
	}
	// Known vector from the Bitcoin base58 test suite.
	if got := Base58Encode([]byte("hello world")); got != "StV1DL6CwTryKyV" {
		t.Fatalf("encode hello world = %s", got)
	}
	if _, err := Base58Decode("0OIl"); err == nil {
		t.Fatal("expected error for invalid characters")
	}
}

func TestKeyStringsRoundTrip(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	k2, err := ParseSecretKey(k.SecretKeyString())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k.Public, k2.Public) {
		t.Fatal("public keys differ after round trip")
	}
	pub, err := ParsePublicKey(k.PublicKeyString())
	if err != nil || !bytes.Equal(pub, k.Public) {
		t.Fatal("public key string round trip failed")
	}
	seed := k.Private.Seed()
	k3, err := ParseSecretKey("ed25519:" + Base58Encode(seed))
	if err != nil || !bytes.Equal(k3.Public, k.Public) {
		t.Fatal("seed form not accepted")
	}
	if _, err := ParseSecretKey("secp256k1:abc"); err == nil {
		t.Fatal("expected error for other curves")
	}
}

func TestParseNEAR(t *testing.T) {
	v := MustNEAR("0.01")
	want, _ := new(big.Int).SetString("10000000000000000000000", 10)
	if v.Cmp(want) != 0 {
		t.Fatalf("0.01 NEAR = %s", v)
	}
	if MustNEAR("3").Cmp(new(big.Int).Mul(big.NewInt(3), Yocto)) != 0 {
		t.Fatal("3 NEAR wrong")
	}
}

func fixedKey() KeyPair {
	seed := sha256.Sum256([]byte("mergoshi test key"))
	priv := ed25519.NewKeyFromSeed(seed[:])
	return KeyPair{Public: priv.Public().(ed25519.PublicKey), Private: priv}
}

// The transfer layout is checked byte by byte against the NEAR borsh schema.
func TestTransferTransactionLayout(t *testing.T) {
	k := fixedKey()
	var bh [32]byte
	bh[0] = 7
	tx := Transaction{
		SignerID: "a.testnet", PublicKey: k.Public, Nonce: 5,
		ReceiverID: "b.testnet", BlockHash: bh,
		Actions: []Action{Transfer{Deposit: big.NewInt(1)}},
	}
	got := tx.Bytes()
	var want []byte
	want = append(want, 9, 0, 0, 0)
	want = append(want, "a.testnet"...)
	want = append(want, 0)
	want = append(want, k.Public...)
	want = append(want, 5, 0, 0, 0, 0, 0, 0, 0)
	want = append(want, 9, 0, 0, 0)
	want = append(want, "b.testnet"...)
	want = append(want, bh[:]...)
	want = append(want, 1, 0, 0, 0) // one action
	want = append(want, 3)          // Transfer
	want = append(want, 1)
	want = append(want, make([]byte, 15)...)
	if !bytes.Equal(got, want) {
		t.Fatalf("layout mismatch\n got %s\nwant %s", hex.EncodeToString(got), hex.EncodeToString(want))
	}
	signed, hash := tx.Sign(k)
	if hash != sha256.Sum256(got) {
		t.Fatal("hash is not sha256 of the body")
	}
	sig := signed[len(got)+1:]
	if signed[len(got)] != 0 || !ed25519.Verify(k.Public, hash[:], sig) {
		t.Fatal("signature does not verify")
	}
}

func TestFunctionCallAndAddKeyLayout(t *testing.T) {
	k := fixedKey()
	var w borsh
	FunctionCall{Method: "gather", Args: []byte(`{}`), Gas: 30 * TGas}.encode(&w)
	want := []byte{2, 6, 0, 0, 0}
	want = append(want, "gather"...)
	want = append(want, 2, 0, 0, 0, '{', '}')
	want = append(want, 0x00, 0xe0, 0x57, 0xeb, 0x48, 0x1b, 0x00, 0x00) // 30e12 = 0x1b48eb57e000, little endian
	want = append(want, make([]byte, 16)...)
	if !bytes.Equal(w.buf, want) {
		t.Fatalf("function call\n got %x\nwant %x", w.buf, want)
	}

	w = borsh{}
	AddKey{PublicKey: k.Public, Permission: &FunctionCallPermission{ReceiverID: "m.testnet"}}.encode(&w)
	want = []byte{5, 0}
	want = append(want, k.Public...)
	want = append(want, make([]byte, 8)...) // nonce
	want = append(want, 0, 0)               // FunctionCall permission, no allowance
	want = append(want, 9, 0, 0, 0)
	want = append(want, "m.testnet"...)
	want = append(want, 0, 0, 0, 0) // no method names
	if !bytes.Equal(w.buf, want) {
		t.Fatalf("add key\n got %x\nwant %x", w.buf, want)
	}
}

func TestDelegateSigningHashUsesNep461Prefix(t *testing.T) {
	k := fixedKey()
	d := DelegateAction{
		SenderID: "p.testnet", ReceiverID: "m.testnet",
		Actions: []Action{CallArgs("gather", map[string]string{"place": "grove"}, 30)},
		Nonce:   11, MaxBlockHeight: 99, PublicKey: k.Public,
	}
	var w borsh
	d.encode(&w)
	prefix := []byte{0x6e, 0x01, 0x00, 0x40} // 2^30 + 366, little endian
	want := sha256.Sum256(append(prefix, w.buf...))
	if d.SigningHash() != want {
		t.Fatal("signing hash must be sha256(prefix || delegate action)")
	}
	s := SignDelegate(d, k)
	h := d.SigningHash()
	if !ed25519.Verify(k.Public, h[:], s.Signature) {
		t.Fatal("delegate signature does not verify")
	}
	w = borsh{}
	Delegate{Signed: s}.encode(&w)
	if w.buf[0] != 8 {
		t.Fatal("delegate action tag must be 8")
	}
}

func TestFailureMessage(t *testing.T) {
	raw := []byte(`{"ActionError":{"index":0,"kind":{"FunctionCallError":{"ExecutionError":"Smart contract panicked: no energy left today"}}}}`)
	if got := FailureMessage(raw); got != "no energy left today" {
		t.Fatalf("got %q", got)
	}
}
