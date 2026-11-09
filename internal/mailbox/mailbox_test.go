package mailbox

import (
	"sync"
	"testing"
)

func TestPutClaimOnce(t *testing.T) {
	b := Box{Dir: t.TempDir()}
	if err := b.Put(Delivery{ID: "d1", SessionID: "s1", Nonce: "n1", Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	if !b.Pending("s1") {
		t.Fatal("not pending")
	}
	var wg sync.WaitGroup
	got := make([]int, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ds, _ := b.Claim("s1")
			got[i] = len(ds)
		}(i)
	}
	wg.Wait()
	total := 0
	for _, n := range got {
		total += n
	}
	if total != 1 {
		t.Fatalf("delivered %d times, want exactly once", total)
	}
	if b.Pending("s1") {
		t.Fatal("still pending after claim")
	}
}

func TestRejectsUnsafeIDs(t *testing.T) {
	b := Box{Dir: t.TempDir()}
	if err := b.Put(Delivery{ID: "d1", SessionID: "../../etc", Text: "x"}); err == nil {
		t.Fatal("accepted a path-traversal session id")
	}
	if ds, _ := b.Claim("../x"); ds != nil {
		t.Fatal("claimed with an unsafe id")
	}
}

func TestOldestFirst(t *testing.T) {
	b := Box{Dir: t.TempDir()}
	b.Put(Delivery{ID: "01B", SessionID: "s", Text: "second"})
	b.Put(Delivery{ID: "01A", SessionID: "s", Text: "first"})
	ds, _ := b.Claim("s")
	if len(ds) != 2 || Text(ds) != "first\n\nsecond" {
		t.Fatalf("got %+v", ds)
	}
}

func TestResendAfterDeliveryIsIgnored(t *testing.T) {
	b := Box{Dir: t.TempDir()}
	d := Delivery{ID: "d1", SessionID: "s", Text: "x"}
	b.Put(d)
	b.Claim("s")
	b.Put(d) // the server resends because the receipt hadn't arrived yet
	if b.Pending("s") {
		t.Fatal("a resend after delivery would deliver twice")
	}
}
