package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"time"

	"cloud.google.com/go/datastore"
	"golang.org/x/net/http2"
)

type Device struct {
	ApiVersion     int       `datastore:"api_version,noindex"`
	App            string    `datastore:"app"`
	Created        time.Time `datastore:"created,noindex"`
	DeviceId       string    `datastore:"device_id,noindex"`
	DeviceInfo     string    `datastore:"device_info,noindex"`
	Environment    string    `datastore:"environment,noindex"`
	Failures       int       `datastore:"failures,noindex"`
	LastSuccess    time.Time `datastore:"last_success,noindex"`
	Platform       string    `datastore:"platform,noindex"`
	Token          string    `datastore:"token"`
	TotalFailures  int       `datastore:"total_failures,noindex"`
	TotalSuccesses int       `datastore:"total_successes,noindex"`
	Updated        time.Time `datastore:"updated,noindex"`
}

type Payload struct {
	AccountID   int64           `json:"account_id"`
	App         string          `json:"app"`
	Data        json.RawMessage `json:"data"`
	DeviceToken string          `json:"device_token"`
	Environment string          `json:"environment"`
}

type PushError struct {
	Body       []byte
	StatusCode int
}

func (pe PushError) Error() string {
	return fmt.Sprintf("HTTP %d (%s)", pe.StatusCode, pe.Body)
}

func (pe PushError) Permanent() bool {
	return pe.StatusCode == 400 || pe.StatusCode == 410
}

func (pe PushError) Retryable() bool {
	return pe.StatusCode == 429 || pe.StatusCode == 500 || pe.StatusCode == 503
}

type ClientMap map[string]*http.Client

func (m ClientMap) Create(app string) *http.Client {
	if _, ok := m[app]; ok {
		panic("tried to overwrite existing client")
	}
	client := NewClient(app)
	m[app] = client
	return client
}

var (
	store     *datastore.Client
	clients   = make(ClientMap)
	ctx       = context.Background()
	timestamp = time.Now()
)

const (
	ProjectId     = "roger-api"
	AppleHost     = "https://api.push.apple.com"
	AppleHostDev  = "https://api.development.push.apple.com"
	DefaultPort   = "8080"
	MaxRetries    = 3
	PingFrequency = time.Second
	PingThreshold = time.Minute
)

func main() {
	// Set up the Datastore client.
	var err error
	store, err = datastore.NewClient(ctx, ProjectId)
	if err != nil {
		log.Fatalf("Failed to create Datastore client (datastore.NewClient: %v)", err)
	}

	// Set up the APNS clients.
	clients.Create("cam.reaction.ReactionCam")

	port := DefaultPort
	if s := os.Getenv("PORT"); s != "" {
		port = s
	}

	http.HandleFunc("/ping", pingHandler)
	http.HandleFunc("/v1/push", pushHandler)

	go pinger()

	// Set up the server.
	log.Printf("Serving on %s...", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("http.ListenAndServe: %v", err)
	}
}

func NewClient(app string) *http.Client {
	cert, err := tls.LoadX509KeyPair(
		fmt.Sprintf("secrets/%s.pem", app),
		fmt.Sprintf("secrets/%s.key", app))
	if err != nil {
		log.Fatalf("Failed to create client for %s: %v", app, err)
	}
	config := &tls.Config{
		Certificates: []tls.Certificate{cert},
	}
	config.BuildNameToCertificate()
	transport := &http.Transport{
		TLSClientConfig: config,
	}
	// Explicitly enable HTTP/2 as TLS-configured clients don't auto-upgrade.
	// See: https://github.com/golang/go/issues/14275
	if err := http2.ConfigureTransport(transport); err != nil {
		log.Fatalf("Failed to configure HTTP/2 for %s client: %v", app, err)
	}
	return &http.Client{
		Timeout:   3 * time.Second,
		Transport: transport,
	}
}

func Push(app, deviceToken, env string, data json.RawMessage) (err error) {
	client, ok := clients[app]
	if !ok {
		err = fmt.Errorf("invalid app \"%s\"", app)
		return
	}
	var url string
	if env == "development" {
		url = fmt.Sprintf("%s/3/device/%s", AppleHostDev, deviceToken)
	} else {
		url = fmt.Sprintf("%s/3/device/%s", AppleHost, deviceToken)
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return
	}
	expiration := time.Now().Add(168 * time.Hour).Unix()
	req.Header.Set("apns-expiration", strconv.FormatInt(expiration, 10))
	req.Header.Set("apns-topic", app)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	timestamp = time.Now()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	// Something went wrong – get the error from body.
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}
	return PushError{Body: body, StatusCode: resp.StatusCode}
}

func pinger() {
	// TODO: Figure out how to ping connections instead of closing.
	for {
		if time.Since(timestamp) > PingThreshold {
			timestamp = time.Now()
			for app := range clients {
				clients[app] = NewClient(app)
			}
		}
		time.Sleep(PingFrequency)
	}
}

// Push with retry.
func push(payload Payload) {
	app := payload.App
	if app == "" {
		log.Printf("Unrecognized app %#v", app)
		return
	}
	accountKey := datastore.IDKey("Account", payload.AccountID, nil)
	deviceKey := datastore.NameKey("Device", payload.DeviceToken, accountKey)
	attempt := 1
	for {
		err := Push(app, payload.DeviceToken, payload.Environment, payload.Data)
		if err, ok := err.(PushError); ok && err.Permanent() {
			if err.Permanent() {
				log.Printf("[%d] PERMANENT FAILURE: %s", payload.AccountID, err)
				if err := store.Delete(ctx, deviceKey); err != nil {
					log.Printf("[%d] FAILED TO DELETE TOKEN: %v", payload.AccountID, err)
				}
			} else if !err.Retryable() {
				log.Printf("[%d] DROPPING NOTIFICATION: %s", payload.AccountID, err)
			}
			log.Printf("[%d] %s", payload.AccountID, string(payload.Data))
			return
		}
		if updateErr := updateDeviceStats(ctx, deviceKey, err == nil); updateErr != nil {
			log.Printf("[%d] FAILED TO UPDATE TOKEN: %v", payload.AccountID, updateErr)
		}
		if err == nil {
			return
		}
		// An error occurred.
		log.Printf("[%d] Failed to push (attempt %d/%d): %s", payload.AccountID, attempt, MaxRetries, err)
		// Exponential backoff.
		if attempt >= MaxRetries {
			log.Printf("[%d] DROPPING NOTIFICATION: exceeded max retries", payload.AccountID)
			log.Printf("[%d] %s", payload.AccountID, string(payload.Data))
			return
		}
		time.Sleep(time.Duration(math.Exp2(float64(attempt-1))) * time.Second)
		attempt += 1
	}
}

func updateDeviceStats(ctx context.Context, key *datastore.Key, success bool) error {
	_, err := store.RunInTransaction(ctx, func(tx *datastore.Transaction) error {
		var device Device
		if err := tx.Get(key, &device); err != nil {
			return err
		}
		now := time.Now()
		device.Updated = now
		if success {
			device.LastSuccess = now
			device.TotalSuccesses += 1
			device.Failures = 0
		} else {
			device.Failures += 1
			device.TotalFailures += 1
		}
		_, err := tx.Put(key, &device)
		return err
	})
	return err
}

func pingHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
	fmt.Fprintln(w, "ok")
}

func pushHandler(w http.ResponseWriter, r *http.Request) {
	scanner := bufio.NewScanner(r.Body)
	for scanner.Scan() {
		var payload Payload
		if err := json.Unmarshal(scanner.Bytes(), &payload); err != nil {
			log.Printf("Failed to parse JSON: %s | %s", err, scanner.Text())
			continue
		}
		go push(payload)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("Failed to read data: %s", err)
	}
}
