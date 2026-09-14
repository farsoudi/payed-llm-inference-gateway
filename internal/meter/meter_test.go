package meter

import "testing"

func TestSetPreservesReservations(t *testing.T) {
	cache := NewBalanceCache()
	cache.Get("key", 100)
	if !cache.Reserve("key", 100, 60) {
		t.Fatal("reserve failed")
	}
	cache.Set("key", 150)
	if got := cache.Get("key", 0); got != 90 {
		t.Fatalf("available after set = %d, want 90", got)
	}
}

func TestAfterDebitUsesDurableBalance(t *testing.T) {
	cache := NewBalanceCache()
	cache.Get("key", 100)
	cache.Reserve("key", 100, 60)
	cache.AfterDebit("key", 140, 60)
	if got := cache.Get("key", 0); got != 140 {
		t.Fatalf("available after debit = %d, want 140", got)
	}
}
