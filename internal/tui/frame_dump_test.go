package tui

import (
	"os"
	"testing"
	"time"

	"github.com/d0lim/aaswap/internal/swap"
	"github.com/d0lim/aaswap/internal/usage"
	"github.com/d0lim/aaswap/internal/usagestore"
)

// TestDumpFrame prints a representative frame. Not an assertion — a way to
// look at the layout. Run with -run TestDumpFrame -v.
func TestDumpFrame(t *testing.T) {
	if os.Getenv("AASWAP_DUMP_FRAME") == "" {
		t.Skip("set AASWAP_DUMP_FRAME=1 to print a frame")
	}
	m := fixture(t,
		[]swap.AccountView{
			{Name: "1", IsActive: true, Account: &swap.Account{
				Email: "work@example.com", OrganizationName: "Acme"}},
			{Name: "2", Account: &swap.Account{Email: "spare@example.com"}},
			{Name: "3", Account: &swap.Account{Email: "burned@example.com", Disabled: true}},
			{Name: "4", Account: &swap.Account{Email: "dead@example.com"}},
			{Name: "5", Account: &swap.Account{Email: "key@example.com"}},
		},
		map[string]usagestore.Entry{
			"1": {FetchedAt: testNow, LastGood: &usage.Result{
				FiveHour: window(62, 6*time.Hour+27*time.Minute),
				SevenDay: window(31, 32*time.Hour)}},
			"2": {FetchedAt: testNow, LastGood: &usage.Result{
				FiveHour: window(11, 3*time.Hour+50*time.Minute),
				SevenDay: window(19, 70*time.Hour)}},
			"3": {FetchedAt: testNow.Add(-9 * time.Minute), LastGood: &usage.Result{
				FiveHour: window(97, 41*time.Minute),
				SevenDay: window(88, 20*time.Hour)}},
			"4": {Sentinel: swap.SentinelReloginRequired},
			"5": {Sentinel: swap.SentinelAPIKey},
		})
	m.watch = true
	m.status = "Activated Account 1 (work@example.com)"
	t.Log("\n" + m.View().Content + "\n")

	// The overlays, which the dashboard frame never shows together.
	token, _ := m.askAddToken()
	typed := token.(Model)
	typed.modal.input = "sk-ant-api03-abcdefghijklmnop"
	t.Log("\n" + typed.View().Content + "\n")

	confirm, _ := m.handleLiveProbed(liveProbedMsg{state: swap.LiveState{
		LoggedIn: true, Identity: swap.LiveIdentity{
			Email: "new@example.com", OrganizationName: "Acme"}}})
	t.Log("\n" + confirm.(Model).View().Content + "\n")
}
