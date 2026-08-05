package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/joho/godotenv"
)

const defaultGraphqlURL = "https://worldofwarcraft.blizzard.com/en-us/graphql"

type realm struct {
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	Online     bool   `json:"online"`
	Population *struct {
		Name string `json:"name"`
	} `json:"population"`
}

type graphqlResponse struct {
	Data struct {
		Realms []realm `json:"Realms"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func fetchRealms(ctx context.Context, graphqlURL, persistedQueryHash, region string) ([]realm, error) {
	reqBody := map[string]any{
		"operationName": "GetRealmStatusData",
		"variables": map[string]any{
			"input": map[string]any{
				"compoundRegionGameVersionSlug": region,
			},
		},
		"extensions": map[string]any{
			"persistedQuery": map[string]any{
				"version":    1,
				"sha256Hash": persistedQueryHash,
			},
		},
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, graphqlURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/graphql-response+json,application/json;q=0.9")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:153.0) Gecko/20100101 Firefox/153.0")
	req.Header.Set("Referer", fmt.Sprintf("https://worldofwarcraft.blizzard.com/en-us/game/status/%s", region))
	req.Header.Set("Origin", "https://worldofwarcraft.blizzard.com")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(respBytes))
	}

	var gqlResp graphqlResponse
	if err := json.Unmarshal(respBytes, &gqlResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if len(gqlResp.Errors) > 0 {
		msgs := make([]string, 0, len(gqlResp.Errors))
		for _, e := range gqlResp.Errors {
			msgs = append(msgs, e.Message)
		}
		return nil, fmt.Errorf("graphql errors: %s", strings.Join(msgs, "; "))
	}

	return gqlResp.Data.Realms, nil
}

func findRealm(realms []realm, slug string) (realm, bool) {
	for _, r := range realms {
		if r.Slug == slug {
			return r, true
		}
	}
	return realm{}, false
}

// --- state persistence (Cloud Storage, since Cloud Run Jobs containers are ephemeral) ---

func readLastState(ctx context.Context, client *storage.Client, bucket, object string) (online bool, found bool) {
	r, err := client.Bucket(bucket).Object(object).NewReader(ctx)
	if err != nil {
		return false, false
	}
	defer r.Close()

	data, err := io.ReadAll(r)
	if err != nil {
		return false, false
	}
	return strings.TrimSpace(string(data)) == "true", true
}

func writeLastState(ctx context.Context, client *storage.Client, bucket, object string, online bool) error {
	val := "false"
	if online {
		val = "true"
	}
	w := client.Bucket(bucket).Object(object).NewWriter(ctx)
	if _, err := w.Write([]byte(val)); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

// --- discord ---

func notifyDiscord(webhookURL, realmName string, nowOnline bool) error {
	dot, status, color := "https://wow.zamimg.com/images/wow/icons/large/inv_alchemy_90_stone_red.jpg", "OFFLINE", 0xe74c3c
	if nowOnline {
		dot, status, color = "https://wow.zamimg.com/images/wow/icons/large/inv_alchemy_90_stone_green.jpg", "ONLINE", 0x2ecc71
	}
	payload := map[string]any{
		"username":   "Realm Watcher",
		"avatar_url": "https://wow.zamimg.com/images/wow/icons/large/trade_engineering.jpg",
		"embeds": []map[string]any{
			{
				"author": map[string]any{
					"name":     fmt.Sprintf("%s — %s", realmName, status),
					"icon_url": dot,
				},
				"color": color,
			},
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("discord webhook returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func main() {
	_ = godotenv.Load()

	realmSlug := os.Getenv("REALM_SLUG")
	if realmSlug == "" {
		log.Fatal("REALM_SLUG env var is required (e.g. 'ragnaros')")
	}
	region := os.Getenv("REGION")
	if region == "" {
		region = "us"
	}
	webhookURL := os.Getenv("DISCORD_WEBHOOK_URL")
	if webhookURL == "" {
		log.Fatal("DISCORD_WEBHOOK_URL env var is required")
	}
	stateFile := os.Getenv("STATE_FILE")
	if stateFile == "" {
		stateFile = "last_state.txt"
	}
	stateBucket := os.Getenv("STATE_BUCKET")
	if stateBucket == "" {
		log.Fatal("STATE_BUCKET env var is required")
	}
	graphqlURL := os.Getenv("GRAPHQL_URL")
	if graphqlURL == "" {
		graphqlURL = defaultGraphqlURL
	}
	persistedQueryHash := os.Getenv("PERSISTED_QUERY_HASH")
	if persistedQueryHash == "" {
		log.Fatal("PERSISTED_QUERY_HASH env var is required")
	}

	ctx := context.Background()

	storageClient, err := storage.NewClient(ctx)
	if err != nil {
		log.Fatalf("create storage client: %v", err)
	}
	defer storageClient.Close()

	realms, err := fetchRealms(ctx, graphqlURL, persistedQueryHash, region)
	if err != nil {
		log.Fatalf("fetch realms: %v", err)
	}

	r, found := findRealm(realms, realmSlug)
	if !found {
		log.Fatalf("realm with slug %q not found in region %q", realmSlug, region)
	}

	lastOnline, hadState := readLastState(ctx, storageClient, stateBucket, stateFile)

	log.Printf("realm=%s online=%v (hadState=%v, previous known=%v )", r.Name, r.Online, hadState, lastOnline)

	if !hadState || lastOnline != r.Online {
		if err := notifyDiscord(webhookURL, r.Name, r.Online); err != nil {
			log.Fatalf("notify discord: %v", err)
		}
		log.Println("status changed, discord notified")
	} else {
		log.Println("no change, skipping notification")
	}

	if err := writeLastState(ctx, storageClient, stateBucket, stateFile, r.Online); err != nil {
		log.Fatalf("write state: %v", err)
	}
}
