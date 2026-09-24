package cosy

import (
	"bytes"
	"testing"
)

// TestEncodeDecodeRoundTrip proves the permuted-base64 codec is reversible for
// a range of payload lengths (the rearrangement uses n/3, so exercise several).
func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := [][]byte{
		[]byte("hello world"),
		[]byte(`{"payload":"abc","encodeVersion":"1"}`),
		[]byte("a"),
		[]byte("ab"),
		[]byte("abc"),
		bytes.Repeat([]byte{0x00, 0x01, 0xfe, 0xff}, 32),
	}
	for _, in := range cases {
		enc, err := Encode(in)
		if err != nil {
			t.Fatalf("Encode(%q) error: %v", in, err)
		}
		out, err := Decode(enc)
		if err != nil {
			t.Fatalf("Decode error for input %q: %v", in, err)
		}
		if !bytes.Equal(in, out) {
			t.Fatalf("round-trip mismatch: got %q want %q", out, in)
		}
	}
}

// TestSignLegacyFixedDate pins the legacy signature to the documented formula
// md5hex("cosy&d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw==&<date>").
func TestSignLegacyFixedDate(t *testing.T) {
	const date = "Mon, 01 Jan 2024 00:00:00 GMT"
	want := Md5Hex("cosy&d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw==&" + date)
	if got := SignLegacy(date); got != want {
		t.Fatalf("SignLegacy = %s, want %s", got, want)
	}
	// Guard the constants themselves so a regression in either is caught.
	if AppCode != "cosy" || SecretB64 != "d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw==" {
		t.Fatalf("signing constants drifted: AppCode=%q SecretB64=%q", AppCode, SecretB64)
	}
}

// TestFingerprintDeterministicEmptySalt asserts the exact derivations for a
// fixed seed under an EMPTY install salt, so any change to the derivation
// algorithm is caught. These vectors match the hub qoder_fingerprint.py.
func TestFingerprintDeterministicEmptySalt(t *testing.T) {
	SetInstallSalt("")
	const seed = "test-uid-123"

	if got := DeriveMachineID(seed); got != "005b8945c0659064f8d25299980a27b3" {
		t.Errorf("DeriveMachineID = %s", got)
	}
	if got := DeriveSessionID(seed); got != "e1527624132e0a39ebcb328440517365" {
		t.Errorf("DeriveSessionID = %s", got)
	}
	if got := DeriveMachineType(seed); got != "66ea01f7983702b088" {
		t.Errorf("DeriveMachineType = %s", got)
	}
	if got := DeriveMachineToken(seed); got != "zzpUYGGMSPEfJVrGQWHj7SBYaRUMwPMK0B4QN_aqKP0" {
		t.Errorf("DeriveMachineToken = %s", got)
	}
}
