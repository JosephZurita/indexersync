package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	log "github.com/sirupsen/logrus"
	"golift.io/starr"
	"golift.io/starr/prowlarr"
	"golift.io/starr/radarr"
	"golift.io/starr/sonarr"
)

const (
	retryAttempts = 3
	retryDelay    = 2 * time.Second
)

func init() {
	// Load .env if available
	if err := godotenv.Load(); err != nil {
		log.Warn(".env file not found, relying on real environment variables")
	}

	// Configure log level from environment
	levelStr := strings.ToLower(os.Getenv("LOG_LEVEL"))
	if levelStr == "" {
		levelStr = "info"
	}
	level, err := log.ParseLevel(levelStr)
	if err != nil {
		log.Warnf("Invalid LOG_LEVEL '%s', defaulting to info", levelStr)
		level = log.InfoLevel
	}
	log.SetLevel(level)

	// Log as text with full timestamp
	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
}

func main() {
	start := time.Now()
	defer log.Infof("Total runtime: %s", time.Since(start))
	log.Info("=================== IndexerSync Start ===================")

	// Read download client names
	pubClient := os.Getenv("PUB_DLCLIENT_NAME")
	privClient := os.Getenv("PRIV_DLCLIENT_NAME")
	if pubClient == "" || privClient == "" {
		log.Fatal("PUB_DLCLIENT_NAME and PRIV_DLCLIENT_NAME must be set")
	}

	// Fetch Prowlarr indexers
	log.Info("--- Loading Prowlarr indexers ---")
	indexers := loadProwlarrIndexers()
	log.Infof("Loaded %d indexers from Prowlarr", len(indexers))

	// Sync Radarr
	log.Info("--- Radarr Sync Start ---")
	syncRadarr(indexers, pubClient, privClient)
	log.Info("--- Radarr Sync Complete ---")

	// Sync Sonarr
	log.Info("--- Sonarr Sync Start ---")
	syncSonarr(indexers, pubClient, privClient)
	log.Info("--- Sonarr Sync Complete ---")

	log.Info("=================== IndexerSync Completed ===================")
}

// withRetry executes f up to retryAttempts times with delay between attempts
func withRetry(name string, f func() error) error {
	var err error
	for i := 1; i <= retryAttempts; i++ {
		err = f()
		if err == nil {
			log.Debugf("%s succeeded on attempt %d", name, i)
			return nil
		}
		log.Warnf("%s attempt %d/%d failed: %v", name, i, retryAttempts, err)
		time.Sleep(retryDelay)
	}
	return fmt.Errorf("%s failed after %d attempts: %w", name, retryAttempts, err)
}

// splitEnv retrieves and splits an environment variable by comma
func splitEnv(key string) []string {
	if val := os.Getenv(key); val != "" {
		parts := strings.Split(val, ",")
		log.Debugf("%s=%v", key, parts)
		return parts
	}
	log.Debugf("%s is not set or empty", key)
	return nil
}

// loadProwlarrIndexers loads and aggregates indexers from all configured Prowlarr instances
func loadProwlarrIndexers() map[string]*prowlarr.IndexerOutput {
	urls := splitEnv("PROWLARR_URL")
	keys := splitEnv("PROWLARR_KEY")
	if len(urls) != len(keys) {
		log.Fatal("Prowlarr URL entries and Key entries don't match")
	}

	indexers := make(map[string]*prowlarr.IndexerOutput)
	for i := range urls {
		svc := fmt.Sprintf("Prowlarr[%d]", i)
		cfg := starr.New(keys[i], urls[i], 0)
		srv := prowlarr.New(cfg)

		log.Infof("%s: fetching indexers", svc)
		var list []*prowlarr.IndexerOutput
		if err := withRetry(svc+" GetIndexers", func() error { var e error; list, e = srv.GetIndexers(); return e }); err != nil {
			log.Errorf("%s: %v", svc, err)
			continue
		}

		for _, idx := range list {
			indexers[idx.Name] = idx
			log.Debugf("%s: loaded indexer %s (privacy=%s)", svc, idx.Name, idx.Privacy)
		}
	}
	return indexers
}

// syncRadarr synchronizes indexer settings in Radarr based on Prowlarr privacy
func syncRadarr(indexers map[string]*prowlarr.IndexerOutput, pubName, privName string) {
	urls := splitEnv("RADARR_URL")
	keys := splitEnv("RADARR_KEY")
	if len(urls) != len(keys) {
		log.Fatal("Radarr URL entries and Key entries don't match")
	}

	for i := range urls {
		svc := fmt.Sprintf("Radarr[%d]", i)
		cfg := starr.New(keys[i], urls[i], 0)
		srv := radarr.New(cfg)

		// Retrieve instance title via API
		var status *radarr.SystemStatus
		if err := withRetry(svc+" GetStatus", func() error { var e error; status, e = srv.GetSystemStatus(); return e }); err != nil {
			log.Warnf("%s: could not retrieve instance title: %v", svc, err)
		} else {
			svc = fmt.Sprintf("Radarr[%d] (%s)", i, status.InstanceName)
			log.Infof("Using instance title %q for %s", status.InstanceName, svc)
		}

		log.Infof("%s: fetching download clients...", svc)
		var clients []*radarr.DownloadClientOutput
		if err := withRetry(svc+" GetDownloadClients", func() error { var e error; clients, e = srv.GetDownloadClients(); return e }); err != nil {
			log.Errorf("%s: %v", svc, err)
			continue
		}

		// Build a map of client ID → client Name for logging
		clientNameMap := make(map[int64]string)
		for _, c := range clients {
			clientNameMap[c.ID] = c.Name
		}

		pubID, privID := findClientIDsRadarr(clients, pubName, privName)
		if pubID == -1 && privID == -1 {
			log.Warnf("%s: no matching download clients found, skipping", svc)
			continue
		}

		log.Infof("%s: fetching indexers...", svc)
		var idxs []*radarr.IndexerOutput
		if err := withRetry(svc+" GetIndexers", func() error { var e error; idxs, e = srv.GetIndexers(); return e }); err != nil {
			log.Errorf("%s: %v", svc, err)
			continue
		}

		for _, idx := range idxs {
			key := strings.TrimSuffix(idx.Name, " (Prowlarr)")
			pr, ok := indexers[key]
			if !ok {
				continue
			}

			// Determine desired client
			desiredID := privID
			if pr.Privacy == "public" {
				desiredID = pubID
			}

			// Skip if already correctly configured
			if idx.DownloadClientID == desiredID {
				cli := clientNameMap[desiredID]
				log.Infof("%s: indexer %s already set to client %q, skipping update", svc, idx.Name, cli)
				continue
			}

			// Prepare update
			input := &radarr.IndexerInput{
				ID:                      idx.ID,
				Name:                    idx.Name,
				Implementation:          idx.Implementation,
				ConfigContract:          idx.ConfigContract,
				Protocol:                idx.Protocol,
				DownloadClientID:        desiredID,
				EnableAutomaticSearch:   idx.EnableAutomaticSearch,
				EnableInteractiveSearch: idx.EnableInteractiveSearch,
				EnableRss:               idx.EnableRss,
				Priority:                idx.Priority,
				Tags:                    idx.Tags,
				Fields:                  cloneFields(idx.Fields),
			}

			log.Infof("%s: updating indexer %s to client %d (privacy=%s)", svc, idx.Name, desiredID, pr.Privacy)
			if err := withRetry(svc+" UpdateIndexer "+idx.Name, func() error { var e error; _, e = srv.UpdateIndexer(input, false); return e }); err != nil {
				log.Errorf("%s: %v", svc, err)
			}
		}
	}
}

// syncSonarr synchronizes indexer settings in Sonarr based on Prowlarr privacy
func syncSonarr(indexers map[string]*prowlarr.IndexerOutput, pubName, privName string) {
	urls := splitEnv("SONARR_URL")
	keys := splitEnv("SONARR_KEY")
	if len(urls) != len(keys) {
		log.Fatal("Sonarr URL entries and Key entries don't match")
	}

	for i := range urls {
		svc := fmt.Sprintf("Sonarr[%d]", i)
		cfg := starr.New(keys[i], urls[i], 0)
		srv := sonarr.New(cfg)

		// Retrieve instance title via API
		var status *sonarr.SystemStatus
		if err := withRetry(svc+" GetStatus", func() error { var e error; status, e = srv.GetSystemStatus(); return e }); err != nil {
			log.Warnf("%s: could not retrieve instance title: %v", svc, err)
		} else {
			svc = fmt.Sprintf("Sonarr[%d] (%s)", i, status.InstanceName)
			log.Infof("Using instance title %q for %s", status.InstanceName, svc)
		}

		log.Infof("%s: fetching download clients...", svc)
		var clients []*sonarr.DownloadClientOutput
		if err := withRetry(svc+" GetDownloadClients", func() error { var e error; clients, e = srv.GetDownloadClients(); return e }); err != nil {
			log.Errorf("%s: %v", svc, err)
			continue
		}

		// Build a map of client ID → client Name for logging
		clientNameMap := make(map[int64]string)
		for _, c := range clients {
			clientNameMap[c.ID] = c.Name
		}

		pubID, privID := findClientIDsSonarr(clients, pubName, privName)
		if pubID == -1 && privID == -1 {
			log.Warnf("%s: no matching download clients found, skipping", svc)
			continue
		}

		log.Infof("%s: fetching indexers...", svc)
		var idxs []*sonarr.IndexerOutput
		if err := withRetry(svc+" GetIndexers", func() error { var e error; idxs, e = srv.GetIndexers(); return e }); err != nil {
			log.Errorf("%s: %v", svc, err)
			continue
		}

		for _, idx := range idxs {
			key := strings.TrimSuffix(idx.Name, " (Prowlarr)")
			pr, ok := indexers[key]
			if !ok {
				continue
			}

			// Determine desired client
			desiredID := privID
			if pr.Privacy == "public" {
				desiredID = pubID
			}

			// Skip if already correctly configured
			if idx.DownloadClientID == desiredID {
				cli := clientNameMap[desiredID]
				log.Infof("%s: indexer %s already set to client %q, skipping update", svc, idx.Name, cli)
				continue
			}

			// Prepare update
			input := &sonarr.IndexerInput{
				ID:                      idx.ID,
				Name:                    idx.Name,
				Implementation:          idx.Implementation,
				ConfigContract:          idx.ConfigContract,
				Protocol:                idx.Protocol,
				DownloadClientID:        desiredID,
				EnableAutomaticSearch:   idx.EnableAutomaticSearch,
				EnableInteractiveSearch: idx.EnableInteractiveSearch,
				EnableRss:               idx.EnableRss,
				Priority:                idx.Priority,
				Tags:                    idx.Tags,
				Fields:                  cloneFields(idx.Fields),
			}

			log.Infof("%s: updating indexer %s to client %d (privacy=%s)", svc, idx.Name, desiredID, pr.Privacy)
			if err := withRetry(svc+" UpdateIndexer "+idx.Name, func() error { var e error; _, e = srv.UpdateIndexer(input, false); return e }); err != nil {
				log.Errorf("%s: %v", svc, err)
			}
		}
	}
}

// findClientIDsRadarr finds public and private client IDs in Radarr's DownloadClientOutput list
func findClientIDsRadarr(clients []*radarr.DownloadClientOutput, pubName, privName string) (int64, int64) {
	pubID, privID := int64(-1), int64(-1)
	for _, c := range clients {
		switch c.Name {
		case pubName:
			pubID = c.ID
		case privName:
			privID = c.ID
		}
	}
	log.Debugf("Radarr clients: pub=%d priv=%d", pubID, privID)
	return pubID, privID
}

// findClientIDsSonarr finds public and private client IDs in Sonarr's DownloadClientOutput list
func findClientIDsSonarr(clients []*sonarr.DownloadClientOutput, pubName, privName string) (int64, int64) {
	pubID, privID := int64(-1), int64(-1)
	for _, c := range clients {
		switch c.Name {
		case pubName:
			pubID = c.ID
		case privName:
			privID = c.ID
		}
	}
	log.Debugf("Sonarr clients: pub=%d priv=%d", pubID, privID)
	return pubID, privID
}

// cloneFields duplicates FieldOutput to FieldInput
func cloneFields(outputs []*starr.FieldOutput) []*starr.FieldInput {
	inputs := make([]*starr.FieldInput, len(outputs))
	for i, o := range outputs {
		inputs[i] = &starr.FieldInput{Name: o.Name, Value: o.Value}
	}
	return inputs
}
