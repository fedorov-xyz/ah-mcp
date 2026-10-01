package ah

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appie "github.com/gwillem/appie-go"
)

// --- retry policy ---

type retryServer struct {
	*httptest.Server
	hits     atomic.Int64
	statuses []int
}

func newRetryServer(t *testing.T, statuses ...int) *retryServer {
	rs := &retryServer{statuses: statuses}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		n := int(rs.hits.Add(1)) - 1
		status := 200
		if n < len(rs.statuses) {
			status = rs.statuses[n]
		}
		if status == 429 {
			w.Header().Set("Retry-After", "1")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(rs.Close)
	return rs
}

func retryClient(slept *[]time.Duration) *http.Client {
	return &http.Client{Transport: &RetryTransport{
		BaseDelay: 10 * time.Millisecond,
		Sleep: func(_ context.Context, d time.Duration) error {
			*slept = append(*slept, d)
			return nil
		},
	}}
}

func TestRetryReads(t *testing.T) {
	var slept []time.Duration
	rs := newRetryServer(t, 503, 429)
	resp, err := retryClient(&slept).Get(rs.URL + "/x")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("read not retried to success: %v", err)
	}
	if rs.hits.Load() != 3 || len(slept) != 2 {
		t.Fatalf("hits=%d sleeps=%v", rs.hits.Load(), slept)
	}
	if slept[1] != time.Second {
		t.Fatalf("Retry-After not honoured: %v", slept)
	}
	if slept[0] < 5*time.Millisecond || slept[0] > 10*time.Millisecond {
		t.Fatalf("backoff with jitter out of range: %v", slept[0])
	}
}

func TestRetryGivesUpAfterThreeAttempts(t *testing.T) {
	var slept []time.Duration
	rs := newRetryServer(t, 429, 429, 429, 429)
	_, err := retryClient(&slept).Get(rs.URL)
	if !errors.Is(err, ErrRateLimited) || StatusOf(err) != 429 {
		t.Fatalf("err = %v", err)
	}
	if rs.hits.Load() != 3 {
		t.Fatalf("attempts = %d", rs.hits.Load())
	}
}

func TestMutationsRetryOnlyOn429(t *testing.T) {
	var slept []time.Duration
	rs := newRetryServer(t, 503)
	c := retryClient(&slept)
	resp, err := c.Post(rs.URL+"/graphql", "application/json", strings.NewReader(`{"query":"mutation X { a }"}`))
	if err != nil || resp.StatusCode != 503 || rs.hits.Load() != 1 {
		t.Fatalf("mutation retried on 503: hits=%d err=%v", rs.hits.Load(), err)
	}

	rs = newRetryServer(t, 429)
	resp, err = c.Post(rs.URL+"/graphql", "application/json", strings.NewReader(`{"query":"mutation X { a }"}`))
	if err != nil || resp.StatusCode != 200 || rs.hits.Load() != 2 {
		t.Fatalf("mutation not retried on 429: hits=%d err=%v", rs.hits.Load(), err)
	}

	// GraphQL queries are reads.
	rs = newRetryServer(t, 502)
	resp, err = c.Post(rs.URL+"/graphql", "application/json", strings.NewReader(`{"query":"  query Q { a }"}`))
	if err != nil || resp.StatusCode != 200 || rs.hits.Load() != 2 {
		t.Fatalf("query not retried: hits=%d err=%v", rs.hits.Load(), err)
	}
}

func TestStatusErrors(t *testing.T) {
	var slept []time.Duration
	for _, code := range []int{401, 403, 404} {
		rs := newRetryServer(t, code)
		_, err := retryClient(&slept).Get(rs.URL)
		if StatusOf(err) != code || errors.Is(err, ErrRateLimited) {
			t.Errorf("%d: err = %v", code, err)
		}
	}
	if StatusOf(errors.New("x")) != 0 {
		t.Error("plain error has a status")
	}
}

func TestIsMutation(t *testing.T) {
	for _, c := range []struct {
		method, path, body string
		want               bool
	}{
		{"GET", "/mobile-services/x", "", false},
		{"HEAD", "/x", "", false},
		{"POST", "/graphql", `{"query":"query A { b }"}`, false},
		{"POST", "/graphql", `{"query":"mutation A { b }"}`, true},
		{"POST", "/graphql", `not json`, true},
		{"PUT", "/mobile-services/order/v1/items", `{}`, true},
		{"PATCH", "/mobile-services/shoppinglist/v2/items", `{}`, true},
		{"POST", "/mobile-auth/v1/auth/token/refresh", `{}`, true},
	} {
		if got := IsMutation(c.method, c.path, []byte(c.body)); got != c.want {
			t.Errorf("IsMutation(%s %s %s) = %v", c.method, c.path, c.body, got)
		}
	}
}

// --- bonus ---

func TestComputeTiers(t *testing.T) {
	tiers := ComputeTiers(2.00, []DiscountLabel{
		{Code: "DISCOUNT_PERCENTAGE", DefaultDescription: "25% korting", Percentage: 25},
		{Code: "DISCOUNT_X_FOR_Y", DefaultDescription: "2 voor 3.00", Count: 2, Price: 3.00},
		{Code: "DISCOUNT_FIXED_PRICE", DefaultDescription: "voor 1.49", Price: 1.49},
		{Code: "DISCOUNT_X_PLUS_Y_FREE", DefaultDescription: "1+1 gratis", Count: 1, FreeCount: 1},
		{Code: "DISCOUNT_ONE_HALF_PRICE", DefaultDescription: "2e halve prijs"},
		{Code: "DISCOUNT_WEIGHT", DefaultDescription: "per 100 g 0.99", Price: 0.99, Unit: "GRAM"},
		{Code: "DISCOUNT_UNKNOWN", DefaultDescription: "mystery"},
	})
	want := []PriceTier{
		{Description: "25% korting", Count: 1, DiscountPct: 25, PricePerPiece: 1.5, TotalPrice: 1.5},
		{Description: "2 voor 3.00", Count: 2, DiscountPct: 25, PricePerPiece: 1.5, TotalPrice: 3},
		{Description: "voor 1.49", Count: 1, DiscountPct: 25.5, PricePerPiece: 1.49, TotalPrice: 1.49},
		{Description: "1+1 gratis", Count: 2, DiscountPct: 50, PricePerPiece: 1, TotalPrice: 2},
		{Description: "2e halve prijs", Count: 2, DiscountPct: 25, PricePerPiece: 1.5, TotalPrice: 3},
		{Description: "per 100 g 0.99", PricePerPiece: 0.99, Unit: "GRAM"},
		{Description: "mystery"},
	}
	if !reflect.DeepEqual(tiers, want) {
		t.Fatalf("tiers =\n%+v\nwant\n%+v", tiers, want)
	}
	if got := BestTierPrice(tiers); got != 1 {
		t.Fatalf("BestTierPrice = %v (weight tiers must be ignored)", got)
	}
	// Without a base price, structural percentages still come through.
	noBase := ComputeTiers(0, []DiscountLabel{{Code: "DISCOUNT_X_PLUS_Y_FREE", Count: 2, FreeCount: 1}})
	if noBase[0].DiscountPct != 33.3 || noBase[0].PricePerPiece != 0 {
		t.Fatalf("no-base tier = %+v", noBase[0])
	}
	if BestTierPrice(nil) != 0 {
		t.Fatal("empty tiers")
	}
}

func TestOfferPricing(t *testing.T) {
	g := sectionGroupResp{ID: "S1", SegmentDescription: "Alle yoghurt", ExampleFromPrice: 2.00,
		DiscountLabels: []DiscountLabel{{Code: "DISCOUNT_X_PLUS_Y_FREE", Count: 1, FreeCount: 1}}}
	o := g.toOffer()
	if o.BonusPrice != 1 || o.DiscountPct != 50 || o.SegmentID != "S1" {
		t.Fatalf("group offer = %+v", o)
	}
}

func TestSelectPeriods(t *testing.T) {
	meta := &bonusMetadataResp{Periods: []bonusPeriodResp{
		{BonusStartDate: "2026-09-21", BonusEndDate: "2026-09-27"},
		{BonusStartDate: "2026-09-28", BonusEndDate: "2026-10-04", NextPeriodVisibleFrom: "2026-10-02"},
		{BonusStartDate: "2026-10-12", BonusEndDate: "2026-10-18"},
		{BonusStartDate: "2026-10-05", BonusEndDate: "2026-10-11"},
	}}
	cur, next := selectPeriods(meta, "2026-10-01")
	if cur == nil || cur.Start != "2026-09-28" || cur.NextVisibleFrom != "2026-10-02" {
		t.Fatalf("current = %+v", cur)
	}
	if next == nil || next.Start != "2026-10-05" {
		t.Fatalf("next = %+v (must be the earliest upcoming)", next)
	}
	cur, next = selectPeriods(&bonusMetadataResp{Periods: meta.Periods[:2]}, "2026-10-04")
	if cur.Start != "2026-09-28" || next != nil {
		t.Fatalf("last day: cur=%+v next=%+v", cur, next)
	}
}

// --- matching ---

func TestTitleTokens(t *testing.T) {
	got := TitleTokens("AH Biologisch Crème fraîche 200 ml")
	if !reflect.DeepEqual(got, map[string]bool{"CREME": true, "FRAICHE": true}) {
		t.Fatalf("tokens = %v", got)
	}
}

func TestFindDuplicatePairs(t *testing.T) {
	pairs := FindDuplicatePairs([]NamedItem{
		{ProductID: 1, Title: "Grand' Italia Lasagne all'uovo"},
		{ProductID: 2, Title: "De Cecco Lasagne all'uovo"},
		{ProductID: 3, Title: "Mutti Pelati"},
		{ProductID: 4, Title: "Mutti Passata"},
		{ProductID: 1, Title: "Grand' Italia Lasagne all'uovo"},
		{Title: "lasagne uovo"},
	})
	// 1–2, 1–6, 2–5, 2–6 and 5–6; never 1–5 (same product) or 3–4 (brand only).
	if len(pairs) != 5 {
		t.Fatalf("pairs = %+v", pairs)
	}
	for _, p := range pairs {
		if p.A.ProductID == 3 || p.B.ProductID == 3 {
			t.Fatalf("brand-only overlap flagged: %+v", p)
		}
		if p.A.ProductID == p.B.ProductID && p.A.ProductID != 0 {
			t.Fatalf("same product flagged: %+v", p)
		}
		if s := strings.Join(p.Shared, ","); !strings.Contains(s, "LASAGNE") || !strings.Contains(s, "UOVO") {
			t.Fatalf("shared = %v", p.Shared)
		}
	}
}

// --- history ---

func TestFrequentItems(t *testing.T) {
	order := func(id int, items ...appie.OrderItem) *appie.Order { return &appie.Order{ID: "x", Items: items} }
	item := func(id, qty int, title string, price float64) appie.OrderItem {
		return appie.OrderItem{ProductID: id, Quantity: qty, Product: &appie.Product{ID: id, Title: title, Price: appie.Price{Now: price}}}
	}
	res := []OrderDetailsResult{
		{Ref: OrderRef{OrderID: 1, Date: "2026-09-01"}, Order: order(1, item(10, 2, "Melk", 1.0), item(20, 1, "Brood", 2.0), item(10, 1, "Melk", 1.0))},
		{Ref: OrderRef{OrderID: 2, Date: "2026-09-08"}, Order: order(2, item(10, 1, "Melk nieuw", 1.1))},
		{Ref: OrderRef{OrderID: 3, Date: "2026-09-15"}, Err: errors.New("boom")},
	}
	items, failed := FrequentItems(res)
	if failed != 1 || len(items) != 2 {
		t.Fatalf("items=%+v failed=%d", items, failed)
	}
	if items[0].ProductID != 10 || items[0].OrderCount != 2 || items[0].TotalQty != 4 || items[0].LastOrdered != "2026-09-08" ||
		items[0].Name != "Melk nieuw" || items[0].LastPrice != 1.1 {
		t.Fatalf("top item = %+v", items[0])
	}
}

func TestMergeHistory(t *testing.T) {
	receipts := []ReceiptDetailsResult{{
		Summary: appie.Receipt{TransactionID: "r1", Date: "2026-09-10T10:00:00Z", TotalAmount: 5.19},
		Detail: &appie.Receipt{Items: []appie.ReceiptItem{
			{Description: "TESTMERK HV MELK", Quantity: 1, Amount: 1.19, ProductID: 555, WebshopID: 10},
			{Description: "BROOD", Quantity: 2, Amount: 4.00, ProductID: 556},
		}},
	}, {Err: errors.New("x")}}
	orders := []OrderDetailsResult{{Ref: OrderRef{OrderID: 1, Date: "2026-09-12"}, Order: &appie.Order{Items: []appie.OrderItem{
		{ProductID: 10, Quantity: 2, Product: &appie.Product{Title: "Testmerk Halfvolle melk", Price: appie.Price{Now: 1.19}}},
	}}}}
	products, failed := MergeHistory(receipts, orders)
	if failed != 1 || len(products) != 2 {
		t.Fatalf("products=%+v failed=%d", products, failed)
	}
	milk := products[0]
	if milk.ProductID != 10 || milk.Purchases != 2 || milk.Quantity != 3 || milk.Spend != 3.57 || milk.LastBought != "2026-09-12" ||
		milk.Name != "Testmerk Halfvolle melk" || !reflect.DeepEqual(milk.Sources, []string{"receipt", "online_order"}) {
		t.Fatalf("merged milk = %+v", milk)
	}
}

func TestDiscountCategory(t *testing.T) {
	for in, want := range map[string]string{"KOOPZEGELS": "koopzegels", "MIJN AH MILES": "miles", "bio premium": "premium", "ZAANLANDER": "bonus"} {
		if got := DiscountCategory(in); got != want {
			t.Errorf("DiscountCategory(%q) = %q", in, got)
		}
	}
}

// --- basket ---

func TestListEntriesAndActiveOrder(t *testing.T) {
	b := Basket{
		ItemsInList: []BasketItem{
			{ID: 1, Quantity: 2, Position: 2, Product: BasketProduct{ID: 100, Title: "Melk"}},
			{ID: 200, Quantity: 1, Position: 1, IsStrikethrough: true},
		},
		Notes: []BasketNote{{Description: "brood", Position: 3}},
	}
	got := b.ListEntries()
	refs := []string{got[0].Ref, got[1].Ref, got[2].Ref}
	if !reflect.DeepEqual(refs, []string{"product:200", "product:100", "note:0"}) {
		t.Fatalf("refs = %v", refs)
	}
	if !got[0].Checked || got[2].Quantity != 1 || got[2].Kind != "note" {
		t.Fatalf("entries = %+v", got)
	}
	if b.HasActiveOrder() {
		t.Fatal("list-only basket detected as active order")
	}
	b.ItemsInOrder = []BasketItem{}
	if !b.HasActiveOrder() {
		t.Fatal("itemsInOrder present but no active order")
	}
	if !(Basket{Summary: BasketSummary{DeliveryDate: "2026-10-03"}}).HasActiveOrder() {
		t.Fatal("delivery date must mean an active order")
	}
}

func TestCheckMutationStatus(t *testing.T) {
	if checkMutationStatus("op", "", "") != nil || checkMutationStatus("op", "success", "") != nil {
		t.Fatal("success rejected")
	}
	if err := checkMutationStatus("op", "FAILED", "nope"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("failure: %v", err)
	}
}

// Regression: the same product on several receipts must form one row, even
// with differing POS ids or a webshop mapping on only some receipts.
func TestMergeHistoryJoinsSameProduct(t *testing.T) {
	rcpt := func(day string, items ...appie.ReceiptItem) ReceiptDetailsResult {
		return ReceiptDetailsResult{Summary: appie.Receipt{Date: day}, Detail: &appie.Receipt{Items: items}}
	}
	products, _ := MergeHistory([]ReceiptDetailsResult{
		rcpt("2026-09-01", appie.ReceiptItem{Description: "TERRA CHIA", Quantity: 1, Amount: 3.75, ProductID: 701}),
		rcpt("2026-09-08", appie.ReceiptItem{Description: "Terra  chia ", Quantity: 2, Amount: 7.50, ProductID: 702}),
		rcpt("2026-09-10", appie.ReceiptItem{Description: "MELK", Quantity: 1, Amount: 1.19, ProductID: 555, WebshopID: 100001}),
		rcpt("2026-09-12", appie.ReceiptItem{Description: "MELK", Quantity: 1, Amount: 1.19, ProductID: 555}), // mapping failed here
	}, nil)
	if len(products) != 2 {
		t.Fatalf("products = %+v", products)
	}
	for _, p := range products {
		switch {
		case p.ProductID == 100001:
			if p.Purchases != 2 {
				t.Errorf("milk = %+v", p)
			}
		case p.Name == "TERRA CHIA" || p.Name == "Terra  chia ":
			if p.Purchases != 2 || p.Spend != 11.25 || p.Quantity != 3 {
				t.Errorf("chia = %+v", p)
			}
		default:
			t.Errorf("unexpected row %+v", p)
		}
	}
}

func TestLimitTransportCapsConcurrencyPerCaller(t *testing.T) {
	var inFlight, peak atomic.Int64
	perToken := map[string]*atomic.Int64{"Bearer a": {}, "Bearer b": {}}
	var peakA atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		if r.Header.Get("Authorization") == "Bearer a" {
			c := perToken["Bearer a"].Add(1)
			for p := peakA.Load(); c > p && !peakA.CompareAndSwap(p, c); p = peakA.Load() {
			}
			defer perToken["Bearer a"].Add(-1)
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
	}))
	defer srv.Close()
	lt := &LimitTransport{Max: 3}
	client := &http.Client{Transport: lt}
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", srv.URL, nil)
			req.Header.Set("Authorization", map[bool]string{true: "Bearer a", false: "Bearer b"}[i%2 == 0])
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	if peakA.Load() > 3 {
		t.Fatalf("caller a had %d requests in flight, max 3", peakA.Load())
	}
	if peak.Load() <= 3 {
		t.Fatalf("callers did not run independently (peak %d)", peak.Load())
	}
	if len(lt.slots) != 0 {
		t.Fatalf("slots not released: %d", len(lt.slots))
	}
}

func TestByMonth(t *testing.T) {
	got := ByMonth([]appie.Receipt{
		{Date: "2026-09-27T10:00:00Z", TotalAmount: 10.005},
		{Date: "2026-09-01T10:00:00Z", TotalAmount: 5},
		{Date: "2026-08-15T10:00:00Z", TotalAmount: 7.5},
	}, []OrderRef{{OrderID: 1, Date: "2026-09-10", Total: 100}, {OrderID: 2, Total: 20}})
	want := []MonthStat{
		{Month: "2026-09", Receipts: 2, ReceiptSpend: 15.01, OnlineOrders: 1, OnlineSpend: 100, Purchases: 3, TotalSpend: 115.01},
		{Month: "2026-08", Receipts: 1, ReceiptSpend: 7.5, Purchases: 1, TotalSpend: 7.5},
		{Month: "unknown", OnlineOrders: 1, OnlineSpend: 20, Purchases: 1, TotalSpend: 20},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ByMonth =\n%+v\nwant\n%+v", got, want)
	}
}

// Regression (real AH data): "2 voor 3.00" group deals quote both example
// prices for the set, so the tier must compare per-piece prices.
func TestGroupXForYTierUsesPiecePrice(t *testing.T) {
	g := sectionGroupResp{ID: "P1", SegmentDescription: "Knorr Good Noodles", DiscountDescription: "2 voor 3.00",
		ExampleFromPrice: 4.38, ExampleForPrice: 3.00,
		DiscountLabels: []DiscountLabel{{Code: "DISCOUNT_X_FOR_Y", DefaultDescription: "2 voor 3.00", Count: 2, Price: 3.00}}}
	o := g.toOffer()
	if o.DiscountPct != 31.5 || len(o.Tiers) != 1 || o.Tiers[0].DiscountPct != 31.5 || o.Tiers[0].PricePerPiece != 1.5 || o.PriceQuantity != 2 {
		t.Fatalf("offer = %+v", o)
	}
	// Live data: Zanetti "2e halve prijs" quoted as van 8.58 voor 6.44 (2 pieces).
	z := sectionGroupResp{ID: "Z", ExampleFromPrice: 8.58, ExampleForPrice: 6.44,
		DiscountLabels: []DiscountLabel{{Code: "DISCOUNT_ONE_HALF_PRICE", DefaultDescription: "2e halve prijs", Count: 2}}}
	if oz := z.toOffer(); oz.PriceQuantity != 2 || oz.Tiers[0].PricePerPiece != 3.22 || oz.Tiers[0].TotalPrice != 6.44 || oz.Tiers[0].DiscountPct != 25 {
		t.Fatalf("zanetti = %+v", oz)
	}
	// Per-piece from-price with a set for-price (for > from): left alone.
	pp := sectionGroupResp{ID: "PP", ExampleFromPrice: 2.19, ExampleForPrice: 3.00,
		DiscountLabels: []DiscountLabel{{Code: "DISCOUNT_X_FOR_Y", Count: 2, Price: 3.00}}}
	if op := pp.toOffer(); op.PriceQuantity != 0 || op.Tiers[0].DiscountPct != 31.5 {
		t.Fatalf("per-piece = %+v", op)
	}
	// Labels without price data produce no tier.
	g2 := sectionGroupResp{ID: "P2", ExampleFromPrice: 3, ExampleForPrice: 2,
		DiscountLabels: []DiscountLabel{{Code: "DISCOUNT_AMOUNT_OFF", DefaultDescription: "€1.00 euro korting"}}}
	if o2 := g2.toOffer(); o2.Tiers != nil || o2.DiscountPct != 33.3 {
		t.Fatalf("offer2 = %+v", o2)
	}
}

// Regression (real order): same brand or line with different products is
// not a duplicate; different brands of the same product still are.
func TestFindDuplicatePairsIgnoresBrandOnlyOverlap(t *testing.T) {
	for _, c := range []struct {
		a, b string
		dup  bool
	}{
		{"Fish Tales Tonijn in olijfolie", "Fish Tales Hollandse nieuwe", false},
		{"Knorr Good potatoes broccoli en kaas 4-pack", "Knorr Good potatoes ui & croutons 4-pack", false},
		{"Grand' Italia Lasagne all'uovo", "De Cecco Lasagne all'uovo", true},
		{"AH Halfvolle melk", "AH Halfvolle melk 1,5 liter", true},
		{"Coca-Cola Vanilla zero sugar", "Quaker Cruesli zero sugar added", false},
	} {
		pairs := FindDuplicatePairs([]NamedItem{{ProductID: 1, Title: c.a}, {ProductID: 2, Title: c.b}})
		if (len(pairs) == 1) != c.dup {
			t.Errorf("%q vs %q: pairs=%+v, want dup=%v", c.a, c.b, pairs, c.dup)
		}
	}
}

func TestBasketPriceRegularAndReduced(t *testing.T) {
	for _, c := range []struct {
		now, was, regular, reduced float64
	}{
		{2.54, 2.99, 2.99, 2.54}, // per-piece deal (activated personal 15%)
		{1.79, 1.79, 1.79, 0},    // deal applied to the basket total
		{1.79, 0, 1.79, 0},       // no "was" price
	} {
		p := BasketProductPrice{Now: Money{c.now}, Was: Money{c.was}}
		if p.Regular() != c.regular || p.Reduced() != c.reduced {
			t.Errorf("%+v: regular=%v reduced=%v", c, p.Regular(), p.Reduced())
		}
	}
}

// Regression (live data): percentages come from exact totals, not from the
// rounded per-piece price, and single-piece tiers follow AH's own rounding.
func TestTierPercentagesAndRounding(t *testing.T) {
	// "2 voor 2.99" at 1.89: (3.78-2.99)/3.78 = 20.9%, not 20.6% from 1.50.
	tiers := ComputeTiers(1.89, []DiscountLabel{{Code: "DISCOUNT_X_FOR_Y", Count: 2, Price: 2.99}})
	if tiers[0].DiscountPct != 20.9 || tiers[0].PricePerPiece != 1.5 {
		t.Fatalf("x for y = %+v", tiers[0])
	}
	// Bonus price derived from the tiers keeps the tier's percentage.
	g := sectionGroupResp{ID: "G", ExampleFromPrice: 1.89,
		DiscountLabels: []DiscountLabel{{Code: "DISCOUNT_X_FOR_Y", Count: 2, Price: 2.99}}}
	if o := g.toOffer(); o.BonusPrice != 1.5 || o.DiscountPct != 20.9 {
		t.Fatalf("derived = %+v", o)
	}
	// 25% off 1.97 is 1.4775 → 1.48 by maths; AH charges 1.47: use AH's.
	pct := []DiscountLabel{{Code: "DISCOUNT_PERCENTAGE", DefaultDescription: "25% korting", Percentage: 25}}
	pi := ProductInfo{Product: appie.Product{IsBonus: true, Price: appie.Price{Now: 1.47, Was: 1.97}}, DiscountLabels: pct}
	if got := pi.Tiers(); got[0].PricePerPiece != 1.47 || got[0].TotalPrice != 1.47 || got[0].DiscountPct != 25 {
		t.Fatalf("product tier = %+v", got[0])
	}
	sp := sectionProductResp{WebshopID: 1, PriceBeforeBonus: 1.97, CurrentPrice: 1.47, DiscountLabels: pct}
	if o := sp.toOffer(); o.Tiers[0].PricePerPiece != 1.47 || o.BonusPrice != 1.47 {
		t.Fatalf("section tier = %+v", o)
	}
	// A real price gap (not rounding) leaves the tier alone.
	far := alignSinglePiece([]PriceTier{{Count: 1, PricePerPiece: 1.48, TotalPrice: 1.48}}, 1.20)
	if far[0].PricePerPiece != 1.48 {
		t.Fatalf("far = %+v", far[0])
	}
	// Multi-piece tiers are never aligned to the single-piece price.
	multi := alignSinglePiece([]PriceTier{{Count: 2, PricePerPiece: 1.48, TotalPrice: 2.96}}, 1.47)
	if multi[0].PricePerPiece != 1.48 {
		t.Fatalf("multi = %+v", multi[0])
	}
}

func TestOfferTitleMatch(t *testing.T) {
	for _, c := range []struct {
		product, offer string
		want, subset   bool
	}{
		{"AH Kipgehakt 500 g", "AH Kipgehakt", true, true},                    // whole short offer title
		{"Zanetti Grana Padano geraspt", "Zanetti", true, true},               // brand-wide group offer
		{"Zanetti Grana Padano", "Zanetti Parmigiano Reggiano", false, false}, // brand only
		{"Lasagne all'uovo", "De Cecco lasagne all'uovo", true, false},        // two shared tokens
		{"AH Halfvolle melk", "AH", false, false},                             // offer without tokens
	} {
		shared, subset, ok := OfferTitleMatch(TitleTokens(c.product), TitleTokens(c.offer))
		if ok != c.want || subset != c.subset {
			t.Errorf("%q vs %q: ok=%v subset=%v shared=%v", c.product, c.offer, ok, subset, shared)
		}
	}
}
