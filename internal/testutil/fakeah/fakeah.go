// Package fakeah is an in-process fake of the Albert Heijn login host and
// API, driven by synthetic fixtures, for tests only. It implements the AH
// login redirect to appie://login-exit, token issue and rotating refresh,
// the member query, the GraphQL basket, orders, receipts, bonus and
// product endpoints that ah-mcp uses.
package fakeah

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fedorov-xyz/ah-mcp/internal/util"
)

//go:embed testdata/*.json
var fixtures embed.FS

// Fixed test values.
const (
	LoginCode = "AH-LOGIN-CODE"
	MemberID  = "4242"
)

// ListItem is a product line of the fake shopping list.
type ListItem struct {
	ID       int
	Title    string
	Price    float64
	Quantity int
	Checked  bool
}

// Note is a fake free-text note.
type Note struct {
	Text     string
	Quantity int
}

// Fake is the fake AH server. Exported fields may be changed by tests
// between requests while holding no lock (tests are sequential).
type Fake struct {
	Server *httptest.Server

	// Email returned by the member query.
	Email string
	// ExpiresIn is the AH access-token lifetime in seconds.
	ExpiresIn int
	// RateLimit makes the next N API requests answer 429.
	RateLimit atomic.Int64
	// ServerErrors makes the next N API requests answer 503.
	ServerErrors atomic.Int64
	// ExtraReceipts adds that many generated receipts (all within the last
	// 300 days) to the receipt list, which is paged by offset/limit.
	ExtraReceipts int
	// OpenDelivered adds an open fulfillment delivered yesterday (AH keeps
	// delivered orders open for a while); OpenUndated adds one without a
	// slot date (its date comes from the order details).
	OpenDelivered, OpenUndated bool

	ExchangeCalls atomic.Int64
	RefreshCalls  atomic.Int64

	mu             sync.Mutex
	seq            int
	access         map[string]bool
	refresh        string
	list           []ListItem
	notes          []Note
	activeOrder    bool
	draftOrder     bool
	order          map[int]int
	ops            map[string]int
	orderPuts      []map[string]int
	reqLog         []string
	forbidRefreshs bool
}

// New starts a fake AH server, closed when t ends.
func New(t testing.TB) *Fake {
	f := &Fake{
		Email:     "test.user@example.com",
		ExpiresIn: 3600,
		access:    map[string]bool{},
		ops:       map[string]int{},
		order:     map[int]int{},
		list: []ListItem{
			{ID: 100001, Title: "Testmerk Halfvolle melk", Price: 1.19, Quantity: 2},
			{ID: 200002, Title: "Grand Testo Lasagne all'uovo", Price: 2.29, Quantity: 1},
		},
		notes: []Note{{Text: "brood", Quantity: 1}},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

// URL is the base URL for both the login host and the API.
func (f *Fake) URL() string { return f.Server.URL }

// IssueTokens creates a valid access/refresh pair without the login flow.
func (f *Fake) IssueTokens() (access, refresh string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issueLocked()
}

func (f *Fake) issueLocked() (string, string) {
	f.seq++
	a := fmt.Sprintf("ah-access-%d", f.seq)
	r := fmt.Sprintf("ah-refresh-%d", f.seq)
	f.access[a] = true
	f.refresh = r
	return a, r
}

// SetDraftOrder makes an active order that is not placed yet (a slot was
// chosen in the app): it is not among the open fulfillments and its state is
// NEW.
func (f *Fake) SetDraftOrder(items map[int]int) {
	f.SetActiveOrder(true, items)
	f.mu.Lock()
	f.draftOrder = true
	f.mu.Unlock()
}

// SetActiveOrder switches the fake between "no order" and a placed
// (scheduled) active order holding items.
func (f *Fake) SetActiveOrder(active bool, items map[int]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activeOrder, f.draftOrder = active, false
	f.order = map[int]int{}
	for k, v := range items {
		f.order[k] = v
	}
}

// Ops returns how often each operation was called.
func (f *Fake) Ops(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ops[name]
}

// List returns a copy of the shopping list items.
func (f *Fake) List() ([]ListItem, []Note) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ListItem(nil), f.list...), append([]Note(nil), f.notes...)
}

// Order returns a copy of the active order quantities.
func (f *Fake) Order() map[int]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int]int{}
	for k, v := range f.order {
		out[k] = v
	}
	return out
}

// Requests returns "METHOD path" for every request seen.
func (f *Fake) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqLog...)
}

func (f *Fake) op(name string) {
	f.mu.Lock()
	f.ops[name]++
	f.mu.Unlock()
}

func fixture(name string) []byte {
	b, err := fixtures.ReadFile("testdata/" + name)
	if err != nil {
		panic(err)
	}
	return b
}

// dated replaces DATE_* placeholders relative to today.
func dated(b []byte) []byte {
	now := time.Now()
	r := strings.NewReplacer(
		"DATE_RECENT", now.AddDate(0, 0, -3).Format("2006-01-02"),
		"DATE_OLDER", now.AddDate(0, 0, -20).Format("2006-01-02"),
	)
	return []byte(r.Replace(string(b)))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	switch b := v.(type) {
	case []byte:
		_, _ = w.Write(b)
	default:
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqLog = append(f.reqLog, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	switch r.URL.Path {
	case "/login":
		f.serveLogin(w, r)
		return
	case "/login/js":
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><a href="/assets/x.js">x</a><script>location.href="appie://login-exit?code=%s"</script><form action="%s/login/submit"></form></html>`, LoginCode, f.Server.URL)
		return
	case "/mobile-auth/v1/auth/token":
		f.serveToken(w, r)
		return
	case "/mobile-auth/v1/auth/token/refresh":
		f.serveRefresh(w, r)
		return
	}

	if f.RateLimit.Load() > 0 {
		f.RateLimit.Add(-1)
		w.Header().Set("Retry-After", "0")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"message": "too many requests"})
		return
	}
	if f.ServerErrors.Load() > 0 {
		f.ServerErrors.Add(-1)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "unavailable"})
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	valid := f.access[tok]
	f.mu.Unlock()
	if !valid {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "unauthorized"})
		return
	}

	p := r.URL.Path
	switch {
	case p == "/graphql" && r.Method == http.MethodPost:
		f.serveGraphQL(w, r)
	case p == "/mobile-services/product/search/v2":
		f.op("search")
		writeJSON(w, 200, fixture("search.json"))
	case strings.HasPrefix(p, "/mobile-services/product/detail/v4/fir/"):
		f.op("product")
		if strings.HasSuffix(p, "/100001") {
			writeJSON(w, 200, fixture("product_detail.json"))
		} else {
			writeJSON(w, 404, map[string]string{"message": "not found"})
		}
	case p == "/mobile-services/bonuspage/v3/metadata":
		f.op("bonus_metadata")
		writeJSON(w, 200, bonusMetadata())
	case p == "/mobile-services/bonuspage/v2/section":
		f.op("bonus_section")
		writeJSON(w, 200, fixture("bonus_section.json"))
	case p == "/mobile-services/bonuspage/v1/personal":
		f.op("bonus_personal")
		writeJSON(w, 200, fixture("bonus_personal.json"))
	case p == "/mobile-services/order/v1/summaries/active":
		f.serveOrderSummary(w)
	case p == "/mobile-services/order/v1/items" && r.Method == http.MethodPut:
		f.serveOrderPut(w, r)
	case strings.HasPrefix(p, "/mobile-services/order/v1/") && strings.HasSuffix(p, "/details-grouped-by-taxonomy"):
		f.op("order_details")
		writeJSON(w, 200, dated(fixture("order_details.json")))
	case p == "/mobile-services/shoppinglist/v2/items" && r.Method == http.MethodPatch:
		f.serveNotesPatch(w, r)
	default:
		writeJSON(w, 404, map[string]string{"message": "no such fake endpoint " + p})
	}
}

func (f *Fake) serveLogin(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != "appie-ios" || q.Get("redirect_uri") != "appie://login-exit" || q.Get("response_type") != "code" {
		http.Error(w, "bad login request", http.StatusBadRequest)
		return
	}
	// Mimics AH's cookie attributes so the proxy's sanitising is exercised.
	http.SetCookie(w, &http.Cookie{Name: "ahsession", Value: "s1", Domain: "login.ah.nl", Secure: true, SameSite: http.SameSiteNoneMode, Path: "/"}) //nolint:gosec // test fixture
	w.Header().Set("Content-Security-Policy", "default-src 'self'")
	w.Header().Set("Location", "appie://login-exit?code="+LoginCode)
	w.WriteHeader(http.StatusFound)
}

func (f *Fake) serveToken(w http.ResponseWriter, r *http.Request) {
	f.ExchangeCalls.Add(1)
	var body struct {
		ClientID string `json:"clientId"`
		Code     string `json:"code"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.ClientID != "appie-ios" || body.Code != LoginCode {
		writeJSON(w, 400, map[string]string{"code": "invalid_grant", "message": "bad code"})
		return
	}
	f.mu.Lock()
	a, rt := f.issueLocked()
	f.mu.Unlock()
	writeJSON(w, 200, map[string]any{"access_token": a, "refresh_token": rt, "member_id": MemberID, "expires_in": f.ExpiresIn})
}

func (f *Fake) serveRefresh(w http.ResponseWriter, r *http.Request) {
	f.RefreshCalls.Add(1)
	var body struct {
		RefreshToken string `json:"refreshToken"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if body.RefreshToken == "" || body.RefreshToken != f.refresh || f.forbidRefreshs {
		writeJSON(w, 400, map[string]string{"code": "invalid_grant", "message": "refresh token expired"})
		return
	}
	// Rotation: the old refresh token is now invalid.
	a, rt := f.issueLocked()
	writeJSON(w, 200, map[string]any{"access_token": a, "refresh_token": rt, "expires_in": f.ExpiresIn})
}

// RejectRefreshes makes every refresh fail.
func (f *Fake) RejectRefreshes() {
	f.mu.Lock()
	f.forbidRefreshs = true
	f.mu.Unlock()
}

func bonusMetadata() map[string]any {
	now := time.Now()
	start := now.AddDate(0, 0, -2).Format("2006-01-02")
	end := now.AddDate(0, 0, 4).Format("2006-01-02")
	nextStart := now.AddDate(0, 0, 5).Format("2006-01-02")
	nextEnd := now.AddDate(0, 0, 11).Format("2006-01-02")
	tab := func(d string) []map[string]any {
		return []map[string]any{{"description": "Alle", "urlMetadataList": []map[string]any{
			{"url": "bonuspage/v2/section?category=Zuivel&date=" + d, "count": 3, "bonusType": "NATIONAL", "description": "Zuivel"},
			{"url": "bonuspage/v2/section?category=Persoonlijk&date=" + d, "count": 1, "bonusType": "PERSONAL", "description": "Persoonlijk"},
		}}}
	}
	return map[string]any{"periods": []map[string]any{
		{"bonusStartDate": start, "bonusEndDate": end, "nextPeriodVisibleFrom": now.AddDate(0, 0, 2).Format("2006-01-02"), "tabs": tab(start)},
		{"bonusStartDate": nextStart, "bonusEndDate": nextEnd, "tabs": tab(nextStart)},
	}}
}

func (f *Fake) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, 400, map[string]string{"message": "bad graphql"})
		return
	}
	q := req.Query
	switch {
	case strings.Contains(q, "FetchMember"):
		f.op("member")
		writeJSON(w, 200, []byte(strings.Replace(string(fixture("member.json")), "EMAIL", f.Email, 1)))
	case strings.Contains(q, "basketItemsAdd"):
		f.op("basket_add")
		f.mutateBasket(req.Variables, true)
		writeJSON(w, 200, map[string]any{"data": map[string]any{"basketItemsAdd": map[string]any{"status": "SUCCESS", "result": f.basketJSON()}}})
	case strings.Contains(q, "basketItemsUpdate"):
		f.op("basket_update")
		f.mutateBasket(req.Variables, false)
		writeJSON(w, 200, map[string]any{"data": map[string]any{"basketItemsUpdate": map[string]any{"status": "SUCCESS", "result": f.basketJSON()}}})
	case strings.Contains(q, "MCPBasket"):
		f.op("basket")
		writeJSON(w, 200, map[string]any{"data": map[string]any{"basket": f.basketJSON()}})
	case strings.Contains(q, "status: CLOSED"):
		f.op("closed_orders")
		writeJSON(w, 200, dated(fixture("fulfillments_closed.json")))
	case strings.Contains(q, "orderFulfillments(status: OPEN)"):
		f.op("open_orders")
		writeJSON(w, 200, f.openFulfillments())
	case strings.Contains(q, "FetchPosReceipts"):
		f.op("receipts")
		writeJSON(w, 200, f.receiptsPage(req.Variables))
	case strings.Contains(q, "FetchReceipt"):
		f.op("receipt")
		// Like the real API: an unknown id is a GraphQL error, not a 404.
		if id, _ := req.Variables["id"].(string); !strings.HasPrefix(id, "RCPT-") && !strings.HasPrefix(id, "GEN-") {
			writeJSON(w, 200, map[string]any{"errors": []any{map[string]any{"message": "Receipt not found"}}})
			return
		}
		writeJSON(w, 200, fixture("receipt_detail.json"))
	case strings.HasPrefix(strings.TrimSpace(q), "query Convert"):
		f.op("convert")
		writeJSON(w, 200, map[string]any{"data": map[string]any{"p0": 100001, "p1": -1}})
	case strings.Contains(q, "StoresSearch"):
		f.op("stores")
		writeJSON(w, 200, fixture("stores.json"))
	case strings.Contains(q, "BargainItems"):
		f.op("bargains")
		writeJSON(w, 200, fixture("bargains.json"))
	case strings.Contains(q, "FetchBonusPromotionWithProducts"):
		f.op("bonus_group")
		writeJSON(w, 200, fixture("group.json"))
	case strings.Contains(q, "FetchProduct"):
		f.op("nutrition")
		writeJSON(w, 200, map[string]any{"data": map[string]any{"product": map[string]any{"id": 100001, "tradeItem": map[string]any{
			"nutritions": []any{map[string]any{"nutrients": []any{map[string]any{"type": "ENERGY", "name": "Energie", "value": "200 kJ"}}}}}}}})
	default:
		writeJSON(w, 200, map[string]any{"errors": []any{map[string]any{"message": "fake: unknown operation"}}})
	}
}

func (f *Fake) mutateBasket(vars map[string]any, add bool) {
	items, _ := vars["items"].([]any)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, raw := range items {
		m, _ := raw.(map[string]any)
		id := int(util.ToFloat(m["id"]))
		qty := int(util.ToFloat(m["quantity"]))
		idx := -1
		for i, it := range f.list {
			if it.ID == id {
				idx = i
			}
		}
		if add && f.activeOrder {
			// Like the real API: while an order is active, added products go
			// straight into the order.
			f.order[id] += qty
			continue
		}
		switch {
		case idx >= 0 && add:
			f.list[idx].Quantity += qty
		case idx >= 0 && qty == 0:
			f.list = append(f.list[:idx], f.list[idx+1:]...)
		case idx >= 0:
			f.list[idx].Quantity = qty
		case qty > 0:
			f.list = append(f.list, ListItem{ID: id, Title: fmt.Sprintf("Fictief product %d", id), Price: 1.00, Quantity: qty})
		}
	}
}

func (f *Fake) basketJSON() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := make([]any, 0, len(f.list))
	var total float64
	for i, it := range f.list {
		total += it.Price * float64(it.Quantity)
		list = append(list, map[string]any{"id": it.ID, "quantity": it.Quantity, "position": i + 1, "isStrikethrough": it.Checked,
			"product": map[string]any{"id": it.ID, "title": it.Title, "priceV2": map[string]any{"now": map[string]any{"amount": it.Price}, "was": map[string]any{"amount": it.Price}}}})
	}
	notes := make([]any, 0, len(f.notes))
	for i, n := range f.notes {
		notes = append(notes, map[string]any{"description": n.Text, "quantity": n.Quantity, "position": len(f.list) + i + 1})
	}
	b := map[string]any{
		"id": "basket-1", "itemsInList": list, "notes": notes,
		"summary": map[string]any{"quantity": len(f.list), "price": map[string]any{
			"priceBeforeDiscount": map[string]any{"amount": total}, "priceAfterDiscount": map[string]any{"amount": total},
			"totalPrice": map[string]any{"amount": total}, "discount": map[string]any{"amount": 0}}},
	}
	if f.activeOrder {
		inOrder := []any{}
		for id, qty := range f.order {
			inOrder = append(inOrder, map[string]any{"id": id, "quantity": qty,
				"product": map[string]any{"id": id, "title": titleFor(id), "priceV2": map[string]any{"now": map[string]any{"amount": 1.5}, "was": map[string]any{"amount": 1.5}}}})
		}
		b["itemsInOrder"] = inOrder
		b["summary"].(map[string]any)["shoppingType"] = "DELIVERY"
		b["summary"].(map[string]any)["deliveryDate"] = time.Now().AddDate(0, 0, 2).Format("2006-01-02")
	} else {
		b["itemsInOrder"] = nil
	}
	return b
}

func (f *Fake) openFulfillments() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := []any{}
	if f.OpenDelivered {
		result = append(result, map[string]any{"orderId": 9100, "statusDescription": "Bezorgd", "shoppingType": "DELIVERY",
			"totalPrice": map[string]any{"totalPrice": map[string]any{"amount": 12.0}},
			"delivery":   map[string]any{"status": "DELIVERED", "slot": map[string]any{"date": time.Now().AddDate(0, 0, -1).Format("2006-01-02")}}})
	}
	if f.OpenUndated {
		result = append(result, map[string]any{"orderId": 9200, "statusDescription": "Afgehaald", "shoppingType": "PICKUP",
			"totalPrice": map[string]any{"totalPrice": map[string]any{"amount": 8.0}}, "delivery": map[string]any{}})
	}
	if f.activeOrder && !f.draftOrder {
		result = append(result, map[string]any{"orderId": 7777, "statusDescription": "Wijzigbaar", "shoppingType": "DELIVERY",
			"modifiable": true, "totalPrice": map[string]any{"totalPrice": map[string]any{"amount": 10.0}},
			"delivery": map[string]any{"status": "OPEN", "slot": map[string]any{"date": time.Now().AddDate(0, 0, 2).Format("2006-01-02"),
				"dateDisplay": "overmorgen", "timeDisplay": "18:00 - 20:00"}}})
	}
	return map[string]any{"data": map[string]any{"orderFulfillments": map[string]any{"result": result}}}
}

func (f *Fake) serveOrderSummary(w http.ResponseWriter) {
	f.op("order_summary")
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.activeOrder {
		writeJSON(w, 400, map[string]string{"code": "ORDER_NOT_FOUND", "message": "Order does not exist"})
		return
	}
	products := []any{}
	var total float64
	for id, qty := range f.order {
		total += float64(qty) * 1.5
		products = append(products, map[string]any{"amount": qty, "quantity": qty, "product": map[string]any{"webshopId": id, "title": titleFor(id)}})
	}
	state := "REOPENED"
	if f.draftOrder {
		state = "NEW"
	}
	writeJSON(w, 200, map[string]any{"id": 7777, "state": state, "shoppingType": "DELIVERY",
		"totalPrice":          map[string]any{"priceTotalPayable": total, "priceDiscount": 0.5},
		"deliveryInformation": map[string]any{"deliveryDate": time.Now().AddDate(0, 0, 2).Format("2006-01-02"), "deliveryStartTime": "18:00", "deliveryEndTime": "20:00"},
		"orderedProducts":     products})
}

func titleFor(id int) string {
	switch id {
	case 100001:
		return "Testmerk Halfvolle melk"
	case 200001:
		return "Fictief Lasagne all'uovo"
	case 200002:
		return "Grand Testo Lasagne all'uovo"
	}
	return fmt.Sprintf("Fictief product %d", id)
}

func (f *Fake) serveOrderPut(w http.ResponseWriter, r *http.Request) {
	f.op("order_put")
	if r.Header.Get("appie-current-order-id") == "" {
		writeJSON(w, 400, map[string]string{"message": "no current order header"})
		return
	}
	var body struct {
		Items []struct {
			ProductID int `json:"productId"`
			Quantity  int `json:"quantity"`
		} `json:"items"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.activeOrder {
		writeJSON(w, 400, map[string]string{"message": "Order does not exist"})
		return
	}
	put := map[string]int{}
	for _, it := range body.Items {
		put[strconv.Itoa(it.ProductID)] = it.Quantity
		if it.Quantity == 0 {
			delete(f.order, it.ProductID)
		} else {
			f.order[it.ProductID] = it.Quantity
		}
	}
	f.orderPuts = append(f.orderPuts, put)
	w.WriteHeader(http.StatusNoContent)
}

func (f *Fake) serveNotesPatch(w http.ResponseWriter, r *http.Request) {
	f.op("notes_patch")
	var body struct {
		Items []struct {
			Description string `json:"description"`
			Quantity    int    `json:"quantity"`
		} `json:"items"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range body.Items {
		if it.Quantity == 0 {
			kept := f.notes[:0]
			for _, n := range f.notes {
				if !strings.EqualFold(n.Text, it.Description) {
					kept = append(kept, n)
				}
			}
			f.notes = kept
		} else {
			f.notes = append(f.notes, Note{Text: it.Description, Quantity: it.Quantity})
		}
	}
	writeJSON(w, 200, map[string]any{})
}

// receiptsPage serves posReceiptsPage: the two fixture receipts plus
// ExtraReceipts generated ones, newest first, honouring offset and limit.
func (f *Fake) receiptsPage(vars map[string]any) map[string]any {
	var base struct {
		Data struct {
			PosReceiptsPage struct {
				PosReceipts []map[string]any `json:"posReceipts"`
			} `json:"posReceiptsPage"`
		} `json:"data"`
	}
	_ = json.Unmarshal(dated(fixture("receipts.json")), &base)
	all := base.Data.PosReceiptsPage.PosReceipts
	now := time.Now()
	for i := range f.ExtraReceipts {
		day := now.AddDate(0, 0, -(4 + i%296)).Format("2006-01-02")
		all = append(all, map[string]any{"id": fmt.Sprintf("GEN-%d", i), "dateTime": day + "T12:00:00Z", "totalAmount": map[string]any{"amount": 1.0}})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i]["dateTime"].(string) > all[j]["dateTime"].(string) })
	offset, limit := int(util.ToFloat(vars["offset"])), int(util.ToFloat(vars["limit"]))
	if limit <= 0 {
		limit = 100
	}
	page := []map[string]any{}
	if offset < len(all) {
		page = all[offset:min(offset+limit, len(all))]
	}
	return map[string]any{"data": map[string]any{"posReceiptsPage": map[string]any{"posReceipts": page}}}
}
