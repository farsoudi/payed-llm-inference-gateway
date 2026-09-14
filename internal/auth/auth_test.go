package auth

import (
	"net/http/httptest"
	"testing"
)

func TestKeyRoundTrip(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(key) < 40 {
		t.Fatalf("key too short: %d", len(key))
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	got, err := FromRequest(r)
	if err != nil || got != key {
		t.Fatalf("got %q, %v", got, err)
	}
	if Hash(key) == Hash(key+"x") {
		t.Fatal("different keys have equal hashes")
	}
}
