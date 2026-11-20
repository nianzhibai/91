package mediasig

import (
	"reflect"
	"testing"
	"time"
)

func TestSignatureBinaryRoundTrip(t *testing.T) {
	signature := sample(syntheticVideo(9), 754*time.Second+300*time.Millisecond, 0, 0)
	signature.Frames[3].Informative = false
	data, err := signature.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	if len(data) > 14*len(signature.Frames)+8 {
		t.Fatalf("encoded size = %d bytes for %d frames", len(data), len(signature.Frames))
	}
	var decoded Signature
	if err := decoded.UnmarshalBinary(data); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if !reflect.DeepEqual(&decoded, signature) {
		t.Fatalf("decoded = %+v, want %+v", decoded, signature)
	}
}

func TestSignatureRejectsForeignData(t *testing.T) {
	valid, _ := sample(syntheticVideo(9), time.Minute, 0, 0).MarshalBinary()
	for name, data := range map[string][]byte{
		"empty":     nil,
		"version":   append([]byte{SignatureFormat + 1}, valid[1:]...),
		"truncated": valid[:len(valid)-3],
		"trailing":  append(append([]byte(nil), valid...), 0),
	} {
		var decoded Signature
		if err := decoded.UnmarshalBinary(data); err == nil {
			t.Fatalf("%s data decoded without error", name)
		}
	}
}
