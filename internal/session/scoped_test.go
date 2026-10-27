package session

import (
	"sync"
	"testing"
	"time"
)

func TestScopedKeysAndBindingsStayIsolated(t *testing.T) {
	store := newCountingStore()
	r := routerWith(store, []string{"public"}, time.Minute)
	publicKey := ScopedKey("", "same")
	groupKey := ScopedKey("g1", "same")
	otherKey := ScopedKey("g2", "same")
	if publicKey != "same" || ScopedKey("g1", "") != "" {
		t.Fatal("public or empty keys changed")
	}
	keys := []string{publicKey, groupKey, otherKey, ScopedKey("", groupKey), ScopedKey("g1", "same:tail"), ScopedKey("g1:same", "tail")}
	seen := map[string]bool{}
	for _, key := range keys {
		if seen[key] {
			t.Fatal("scope key collision")
		}
		seen[key] = true
	}
	r.Bind(publicKey, "public")
	r.Bind(groupKey, "group-a")
	r.Bind(otherKey, "other")
	for i := 0; i < 3; i++ {
		if uid, ok := r.ResolveWithCandidates(groupKey, []string{"group-a", "group-b"}); !ok || uid != "group-a" {
			t.Fatalf("group binding=%q ok=%v", uid, ok)
		}
	}
	if uid, ok := r.ResolveWithCandidates(groupKey, []string{"group-b"}); !ok || uid != "group-b" {
		t.Fatalf("unavailable account not replaced: %q %v", uid, ok)
	}
	restored := routerWith(store, []string{"public"}, time.Minute)
	restored.LoadFromStore()
	if uid, ok := restored.ResolveWithCandidates(groupKey, []string{"group-a", "group-b"}); !ok || uid != "group-b" {
		t.Fatalf("group restore=%q ok=%v", uid, ok)
	}
	if uid, ok := restored.Resolve(publicKey); !ok || uid != "public" {
		t.Fatal("group binding changed public binding")
	}
	restored.Unbind(groupKey)
	if uid, ok := restored.ResolveWithCandidates(otherKey, []string{"other"}); !ok || uid != "other" {
		t.Fatal("group unbind changed another group")
	}
	if uid, ok := restored.ResolveWithCandidates(groupKey, nil); ok || uid != "" {
		t.Fatal("empty scoped pool fell back to configured public pool")
	}
}

func TestScopedCandidatesConcurrentResolution(t *testing.T) {
	r := routerWith(newCountingStore(), nil, time.Minute)
	key := ScopedKey("g", "same")
	const n = 32
	results := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			uid, _ := r.ResolveWithCandidates(key, []string{"a", "b", "c"})
			results <- uid
		}()
	}
	wg.Wait()
	close(results)
	first := ""
	for uid := range results {
		if uid == "" {
			t.Fatal("no binding")
		}
		if first == "" {
			first = uid
		}
		if uid != first {
			t.Fatal("concurrent session allocated different accounts")
		}
	}
}
