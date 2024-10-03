package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/baely/balance/pkg/model"
	office "github.com/baely/officetracker/pkg/model"
	"github.com/twmb/franz-go/pkg/kgo"
)

var (
	//go:embed index.html
	indexHTML string

	//go:embed coffee-cup.png
	coffeeCup []byte
)

var (
	officeTrackerAPIKey = os.Getenv("OFFICETRACKER_API_KEY")
	kafkaBrokers        = os.Getenv("KAFKA_BROKERS")
	loc, _              = time.LoadLocation("Australia/Melbourne")
)

var (
	cachedTransaction model.TransactionResource
	indexPage         []byte
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	m := http.NewServeMux()
	refreshPage()
	go transactionListener()
	go updateStatus()
	go dailyPageRefresher()
	m.HandleFunc("/raw", rawHandler)
	m.HandleFunc("/", indexHandler)
	m.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-Type", "image/png")
		w.Header().Add("Cache-Control", "public, max-age=604800, immutable")
		w.Write(coffeeCup)
	})
	s := &http.Server{
		Addr:    ":" + port,
		Handler: m,
	}
	fmt.Printf("listening on port %s\n", port)
	if err := s.ListenAndServe(); err != nil {
		panic(err)
	}
}

func transactionListener() {
	client, err := kgo.NewClient(
		kgo.SeedBrokers(kafkaBrokers),
		kgo.ConsumeTopics("transactions"),
	)
	if err != nil {
		panic(err)
	}
	defer client.Close()

	ctx := context.Background()

	for {
		fetches := client.PollFetches(ctx)
		if errs := fetches.Errors(); len(errs) > 0 {
			panic(fmt.Sprintln(errs))
		}

		iter := fetches.RecordIter()
		for !iter.Done() {
			message := iter.Next()
			var transactionEvent struct {
				Account     model.AccountResource
				Transaction model.TransactionResource
			}
			_ = json.Unmarshal(message.Value, &transactionEvent)
			updatePresence(transactionEvent.Transaction)
		}
	}
}

func rawHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	latestTransaction := getLatest()
	w.Header().Add("Content-Type", "text/plain")
	w.Write([]byte(presentString(latestTransaction)))
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	slog.Info("Request received")
	w.Write(indexPage)
}

func updatePresence(transaction model.TransactionResource) {
	if !check(transaction,
		amountBetween(-700, -400), // between -$7 and -$4
		//timeBetween(6, 12),                // between 6am and 12pm
		weekday(),                         // on a weekday
		notForeign(),                      // not a foreign transaction
		category("restaurants-and-cafes"), // in the restaurants-and-cafes category
	) {
		slog.Warn("Transaction does not meet criteria")
		return
	}

	store(transaction)
}

func present(latestTransaction model.TransactionResource) bool {
	return check(latestTransaction,
		fresh(),
	)
}

func presentString(latestTransaction model.TransactionResource) string {
	if present(latestTransaction) {
		return "yes"
	}
	return "no"
}

type decider func(model.TransactionResource) bool

func check(transaction model.TransactionResource, deciders ...decider) bool {
	for _, d := range deciders {
		if !d(transaction) {
			return false
		}
	}
	return true
}

func amountBetween(minBaseUnits, maxBaseUnits int) decider {
	return func(transaction model.TransactionResource) bool {
		valueInBaseUnits := transaction.Attributes.Amount.ValueInBaseUnits
		return valueInBaseUnits >= minBaseUnits && valueInBaseUnits <= maxBaseUnits
	}
}

func timeBetween(minHour, maxHour int) decider {
	return func(transaction model.TransactionResource) bool {
		hour := transaction.Attributes.CreatedAt.Hour()
		return hour >= minHour && hour <= maxHour
	}
}

func weekday() decider {
	return func(transaction model.TransactionResource) bool {
		day := transaction.Attributes.CreatedAt.Weekday()
		return day >= 1 && day <= 5
	}
}

func fresh() decider {
	return func(transaction model.TransactionResource) bool {
		return isToday(transaction)
	}
}

func notForeign() decider {
	return func(transaction model.TransactionResource) bool {
		return transaction.Attributes.ForeignAmount == nil
	}
}

func category(categoryId string) decider {
	return func(transaction model.TransactionResource) bool {
		return transaction.Relationships.Category.Data.Id == categoryId
	}
}

func isToday(transaction model.TransactionResource) bool {
	now := time.Now().In(loc)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	return transaction.Attributes.CreatedAt.After(midnight)
}

func getLatest() model.TransactionResource {
	return cachedTransaction
}

func must[T any](t T, err error) T {
	if err != nil {
		panic(err)
	}
	return t
}

func getReason(presence bool, t model.TransactionResource) string {
	state := latestStatus
	if !presence && state == office.StateWorkFromOffice {
		return "(but he said he would be)"
	}

	if presence {
		amt := fmt.Sprintf("$%.2f", -float64(t.Attributes.Amount.ValueInBaseUnits)/100.0)
		p1 := fmt.Sprintf("%s at %s", t.Attributes.Description, t.Attributes.CreatedAt.In(loc).Format(time.Kitchen))
		return fmt.Sprintf("<img src=\"/favicon.ico\" />%s on %s", amt, p1)
	}

	return ""
}

var (
	latestStatus = office.State(-1)
)

func updateStatus() {
	ticker := time.Tick(5 * time.Minute)
	for {
		<-ticker
		latestStatus = getOfficeStatus()
	}
}

func getOfficeStatus() office.State {
	uriPattern := "https://iwasintheoffice.com/api/v1/state/%d/%d/%d"
	now := time.Now().In(loc)
	uriStr := fmt.Sprintf(uriPattern, now.Year(), now.Month(), now.Day())
	uri := must(url.Parse(uriStr))
	req := &http.Request{
		Method: http.MethodGet,
		URL:    uri,
		Header: map[string][]string{
			"Authorization": {"Bearer " + officeTrackerAPIKey},
		},
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("Error getting office status: %v", err)
		return office.StateUntracked
	}
	defer resp.Body.Close()
	var stateResp office.GetDayResponse
	if err := json.NewDecoder(resp.Body).Decode(&stateResp); err != nil {
		slog.Error("Error decoding office status: %v", err)
		return office.StateUntracked
	}
	latestStatus = stateResp.Data.State
	return stateResp.Data.State
}

func store(transaction model.TransactionResource) {
	if cachedTransaction.Attributes.CreatedAt.After(transaction.Attributes.CreatedAt) {
		return
	}
	fmt.Printf("Cached transaction updated, %s on %s\n", transaction.Attributes.Description, transaction.Attributes.CreatedAt.Format(time.RFC1123))
	cachedTransaction = transaction
	refreshPage()
}

func refreshPage() {
	latestTransaction := getLatest()
	title := presentString(latestTransaction)
	desc := getReason(present(latestTransaction), latestTransaction)
	indexPage = []byte(fmt.Sprintf(indexHTML, title, desc))
}

func dailyPageRefresher() {
	ticker := make(chan time.Time)
	go runDailyTicker(ticker)
	for {
		<-ticker
		refreshPage()
	}
}

func runDailyTicker(ticker chan<- time.Time) {
	for {
		now := time.Now()
		nextMidnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
		duration := nextMidnight.Sub(now)

		// Sleep until the next midnight
		time.Sleep(duration)

		// Send the tick
		ticker <- time.Now()
	}
}
