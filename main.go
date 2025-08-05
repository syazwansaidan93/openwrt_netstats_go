package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync" // Required for sync.Mutex
	"time"

	_ "github.com/mattn/go-sqlite3" // SQLite driver
)

// --- Configuration Structs ---

// RouterConfig defines the structure for each router's URLs in the config file.
type RouterConfig struct {
	APStatsURL    string `json:"ap_stats"`
	WANStatsURL   string `json:"wan_stats"`
	DHCPLeasesURL string `json:"dhcp_leases"`
}

// Config is a map where keys are router IPs and values are RouterConfig structs.
type Config map[string]RouterConfig

// --- Constants ---

const (
	STATS_DB_NAME = "network_stats.db" // Database file for network traffic statistics
	DHCP_DB_NAME  = "dhcp_leases.db"   // Database file for DHCP lease information
	CONFIG_FILE   = "routers.json"     // Name of the configuration file
)

// --- Data Structs ---

// ClientStats holds parsed WiFi client traffic data.
type ClientStats struct {
	MACAddress string
	RXBytes    int64
	TXBytes    int64
}

// WANStats holds parsed WAN interface traffic data.
type WANStats struct {
	RXBytes int64
	TXBytes int64
}

// DHCPLease holds parsed DHCP lease information.
type DHCPLease struct {
	MACAddress   string
	LeaseEndTime int64
	IPAddress    string
	Hostname     string
	ClientID     string
}

// --- Custom Error for Empty URL ---
var ErrURLEmpty = fmt.Errorf("URL is empty")

// --- Configuration Functions ---

// loadConfig loads router configuration from a JSON file.
// It returns the parsed Config map and an error if any occurs.
func loadConfig(filename string) (Config, error) {
	file, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("error: Configuration file '%s' not found", filename)
		}
		return nil, fmt.Errorf("error opening config file '%s': %w", filename, err)
	}
	defer file.Close() // Ensure the file is closed after reading

	byteValue, err := ioutil.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("error reading config file '%s': %w", filename, err)
	}

	var config Config
	if err := json.Unmarshal(byteValue, &config); err != nil {
		return nil, fmt.Errorf("error: Invalid JSON format in '%s': %w", filename, err)
	}
	return config, nil
}

// --- Database Functions ---

// connectDB establishes a connection to a specified SQLite database.
// It returns a pointer to the sql.DB object and an error if the connection fails.
func connectDB(dbName string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbName)
	if err != nil {
		return nil, fmt.Errorf("database connection error for %s: %w", dbName, err)
	}
	// Ping the database to ensure the connection is active
	if err = db.Ping(); err != nil {
		db.Close() // Close the connection if ping fails
		return nil, fmt.Errorf("database ping error for %s: %w", dbName, err)
	}
	return db, nil
}

// setupStatsDB creates tables for cumulative and monthly stats if they don't exist.
func setupStatsDB(db *sql.DB) error {
	tx, err := db.Begin() // Start a transaction for atomicity
	if err != nil {
		return fmt.Errorf("failed to begin transaction for stats DB setup: %w", err)
	}
	defer tx.Rollback() // Rollback on error, commit manually on success

	// Table to store the last known cumulative values (for incremental calculation)
	_, err = tx.Exec(`
		CREATE TABLE IF NOT EXISTS cumulative_stats (
			id TEXT PRIMARY KEY,
			rx_bytes INTEGER,
			tx_bytes INTEGER
		)
	`)
	if err != nil {
		return fmt.Errorf("error creating cumulative_stats table: %w", err)
	}

	// Table to store the monthly totals
	_, err = tx.Exec(`
		CREATE TABLE IF NOT EXISTS monthly_stats (
			id TEXT PRIMARY KEY,
			rx_bytes INTEGER,
			tx_bytes INTEGER,
			timestamp TEXT
		)
	`)
	if err != nil {
		return fmt.Errorf("error creating monthly_stats table: %w", err)
	}

	return tx.Commit() // Commit the transaction
}

// setupDHCPDB creates the table for DHCP leases if it doesn't exist.
func setupDHCPDB(db *sql.DB) error {
	tx, err := db.Begin() // Start a transaction
	if err != nil {
		return fmt.Errorf("failed to begin transaction for DHCP DB setup: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		CREATE TABLE IF NOT EXISTS dhcp_leases (
			mac_address TEXT PRIMARY KEY,
			lease_end_time INTEGER,
			ip_address TEXT,
			hostname TEXT,
			client_id TEXT,
			timestamp TEXT
		)
	`)
	if err != nil {
		return fmt.Errorf("error creating dhcp_leases table: %w", err)
	}

	return tx.Commit()
}

// resetMonthlyStats resets the monthly stats at the beginning of a new month.
// This checks if the last update was in a different month or year to ensure a robust reset.
func resetMonthlyStats(db *sql.DB, mutex *sync.Mutex) error {
	mutex.Lock()
	defer mutex.Unlock()

	// Check if the monthly table is empty. If so, there's nothing to reset.
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM monthly_stats").Scan(&count)
	if err != nil {
		return fmt.Errorf("error checking monthly_stats table count: %w", err)
	}
	if count == 0 {
		return nil // Table is empty, no reset needed
	}

	var lastUpdateStr string
	err = db.QueryRow("SELECT timestamp FROM monthly_stats ORDER BY timestamp DESC LIMIT 1").Scan(&lastUpdateStr)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil // No rows yet, no reset needed
		}
		return fmt.Errorf("error fetching last update timestamp from monthly_stats: %w", err)
	}

	lastUpdateDate, err := time.Parse("2006-01-02 15:04:05", lastUpdateStr)
	if err != nil {
		return fmt.Errorf("error parsing last update timestamp '%s': %w", lastUpdateStr, err)
	}

	currentDate := time.Now()

	// Check if month or year has changed
	if lastUpdateDate.Month() != currentDate.Month() || lastUpdateDate.Year() != currentDate.Year() {
		_, err := db.Exec(`
			UPDATE monthly_stats
			SET rx_bytes = 0,
				tx_bytes = 0,
				timestamp = ?
		`, currentDate.Format("2006-01-02 15:04:05"))
		if err != nil {
			return fmt.Errorf("error resetting monthly stats: %w", err)
		}
		fmt.Println("Monthly statistics reset due to new month/year.")
	}
	return nil
}

// --- Data Fetching and Parsing Functions ---

// fetchData fetches text data from a given URL with a timeout.
// Returns the content as a string or an error if one occurs.
func fetchData(url string) (string, error) {
	if url == "" {
		return "", ErrURLEmpty // Use the custom error
	}

	// Create a custom HTTP client with a transport that disables keep-alives
	client := &http.Client{
		Timeout: 10 * time.Second, // 10-second timeout for requests
		Transport: &http.Transport{
			DisableKeepAlives: true, // Disable HTTP keep-alives to prevent idle channel issues
		},
	}

	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("error fetching data from %s: %w", url, err)
	}
	defer resp.Body.Close() // Ensure the response body is closed

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP error fetching data from %s: %d - %s", url, resp.StatusCode, resp.Status)
	}

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response body from %s: %w", url, err)
	}

	return string(bodyBytes), nil
}

// parseWiFiStats parses client RX/TX data from the totalwifi.cgi output.
// Returns a slice of ClientStats structs.
func parseWiFiStats(data string) ([]ClientStats, error) {
	if data == "" {
		return nil, nil // No data to parse
	}

	var clients []ClientStats
	lines := strings.Split(strings.TrimSpace(data), "\n")
	for _, line := range lines {
		parts := strings.Fields(line) // Splits by whitespace
		if len(parts) == 3 {
			macAddress := strings.ToLower(parts[0])
			rxBytes, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				fmt.Printf("Error parsing RX bytes for line '%s': %v\n", line, err)
				continue
			}
			txBytes, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil {
				fmt.Printf("Error parsing TX bytes for line '%s': %v\n", line, err)
				continue
			}
			clients = append(clients, ClientStats{
				MACAddress: macAddress,
				RXBytes:    rxBytes,
				TXBytes:    txBytes,
			})
		} else {
			fmt.Printf("Warning: Skipping malformed WiFi stats line: '%s'\n", line)
		}
	}
	return clients, nil
}

// parseWANStats parses WAN RX/TX data from the wan.cgi output.
// Returns a pointer to a WANStats struct.
func parseWANStats(data string) (*WANStats, error) {
	if data == "" {
		return nil, nil
	}

	// Regex to find "wan: RX_BYTES TX_BYTES"
	re := regexp.MustCompile(`wan:\s+(\d+)\s+(\d+)`)
	match := re.FindStringSubmatch(data)

	if len(match) == 3 { // Full match + two capturing groups
		rxBytes, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("error parsing WAN RX bytes from data '%s': %w", data, err)
		}
		txBytes, err := strconv.ParseInt(match[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("error parsing WAN TX bytes from data '%s': %w", data, err)
		}
		return &WANStats{
			RXBytes: rxBytes,
			TXBytes: txBytes,
		}, nil
	}

	return nil, fmt.Errorf("WAN stats pattern not found in data: '%s'", data)
}

// parseDHCPLeases parses DHCP lease data from the dhcp.cgi output.
// Focuses on IPv4 leases for simplicity.
// Returns a slice of DHCPLease structs.
func parseDHCPLeases(data string) ([]DHCPLease, error) {
	if data == "" {
		return nil, nil
	}

	var leases []DHCPLease
	lines := strings.Split(strings.TrimSpace(data), "\n")
	// Regex for IPv4 lease: lease_end_time MAC_ADDRESS IP_ADDRESS HOSTNAME CLIENT_ID
	// Hostname can be '*' or an actual hostname, Client ID can be a MAC or other hex string
	ipv4LeasePattern := regexp.MustCompile(
		`^(\d+)\s+([0-9a-fA-F:]{17})\s+([\d\.]+)\s+(.*?)\s+([\d0-9a-fA-F:]+)$`,
	)

	for _, line := range lines {
		match := ipv4LeasePattern.FindStringSubmatch(line)
		if len(match) == 6 { // Full match + 5 capturing groups
			leaseEndTime, err := strconv.ParseInt(match[1], 10, 64)
			if err != nil {
				fmt.Printf("Error parsing lease end time for line '%s': %v\n", line, err)
				continue
			}
			macAddress := strings.ToLower(match[2])
			ipAddress := match[3]
			hostname := strings.TrimSpace(match[4])
			if hostname == "*" {
				hostname = "Unknown"
			} else {
				// In case hostname has extra info, take only the first word
				hostnameParts := strings.Fields(hostname)
				if len(hostnameParts) > 0 {
					hostname = hostnameParts[0]
				}
			}
			clientID := match[5]

			leases = append(leases, DHCPLease{
				MACAddress:   macAddress,
				LeaseEndTime: leaseEndTime,
				IPAddress:    ipAddress,
				Hostname:     hostname,
				ClientID:     clientID,
			})
		} else {
			fmt.Printf("Warning: Skipping malformed DHCP lease line: '%s'\n", line)
		}
	}
	return leases, nil
}

// --- Database Update Functions ---

// updateTrafficStats calculates incremental traffic and updates the monthly totals.
// This function handles router resets by assuming a reset if new_rx/tx is less than last_rx/tx.
func updateTrafficStats(db *sql.DB, mutex *sync.Mutex, entityID string, newRX, newTX int64) error {
	mutex.Lock() // Acquire lock before database operations
	defer mutex.Unlock() // Release lock when function exits

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction for traffic stats: %w", err)
	}
	defer tx.Rollback() // Rollback on error

	var lastRX, lastTX int64
	// Get the last known cumulative stats
	err = tx.QueryRow("SELECT rx_bytes, tx_bytes FROM cumulative_stats WHERE id = ?", entityID).Scan(&lastRX, &lastTX)

	// Initialize monthly stats if not present
	var monthlyCount int
	err = db.QueryRow("SELECT COUNT(*) FROM monthly_stats WHERE id = ?", entityID).Scan(&monthlyCount)
	if err != nil {
		return fmt.Errorf("error checking monthly stats existence for %s: %w", entityID, err)
	}
	if monthlyCount == 0 {
		_, err = tx.Exec(`
			INSERT INTO monthly_stats (id, rx_bytes, tx_bytes, timestamp)
			VALUES (?, ?, ?, ?)
		`, entityID, 0, 0, time.Now().Format("2006-01-02 15:04:05"))
		if err != nil {
			return fmt.Errorf("error initializing monthly stats for %s: %w", entityID, err)
		}
	}

	var incrementalRX, incrementalTX int64

	if err == sql.ErrNoRows {
		// No previous cumulative stats, this is the first run for this entity.
		incrementalRX = newRX
		incrementalTX = newTX
	} else if err != nil {
		return fmt.Errorf("error fetching cumulative stats for %s: %w", entityID, err)
	} else {
		// Calculate incremental traffic, handling resets
		if newRX >= lastRX {
			incrementalRX = newRX - lastRX
		} else {
			// Router reset detected for RX, assume new_rx is the current total after reset
			incrementalRX = newRX
		}

		if newTX >= lastTX {
			incrementalTX = newTX - lastTX
		} else {
			// Router reset detected for TX, assume new_tx is the current total after reset
			incrementalTX = newTX
		}
	}

	// Update monthly total
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	_, err = tx.Exec(`
		UPDATE monthly_stats
		SET rx_bytes = rx_bytes + ?,
			tx_bytes = tx_bytes + ?,
			timestamp = ?
		WHERE id = ?
	`, incrementalRX, incrementalTX, timestamp, entityID)
	if err != nil {
		return fmt.Errorf("error updating monthly stats for %s: %w", entityID, err)
	}

	// Update the last known cumulative stats with the new values
	_, err = tx.Exec(`
		INSERT OR REPLACE INTO cumulative_stats (id, rx_bytes, tx_bytes)
		VALUES (?, ?, ?)
	`, entityID, newRX, newTX)
	if err != nil {
		return fmt.Errorf("error upserting cumulative stats for %s: %w", entityID, err)
	}

	return tx.Commit() // Commit the transaction
}

// upsertDHCPLeases inserts or updates DHCP leases in the dedicated DHCP database.
func upsertDHCPLeases(db *sql.DB, mutex *sync.Mutex, leases []DHCPLease) error {
	if len(leases) == 0 {
		return nil
	}

	mutex.Lock() // Acquire lock before database operations
	defer mutex.Unlock() // Release lock when function exits

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction for DHCP leases: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT OR REPLACE INTO dhcp_leases (mac_address, lease_end_time, ip_address, hostname, client_id, timestamp)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare statement for DHCP leases: %w", err)
	}
	defer stmt.Close()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	for _, lease := range leases {
		_, err := stmt.Exec(
			lease.MACAddress,
			lease.LeaseEndTime,
			lease.IPAddress,
			lease.Hostname,
			lease.ClientID,
			timestamp,
		)
		if err != nil {
			return fmt.Errorf("error upserting DHCP lease for %s: %w", lease.MACAddress, err)
		}
	}

	return tx.Commit()
}

// --- Main Execution ---

func main() {
	// Loop indefinitely to run the data collection every 30 minutes
	for {
		fmt.Println("Starting data collection cycle...")
		// 1. Load configuration
		routers, err := loadConfig(CONFIG_FILE)
		if err != nil {
			fmt.Printf("Failed to load configuration: %v\n", err)
			// If config loading fails, we might want to sleep and retry, not exit
			time.Sleep(30 * time.Minute)
			continue // Skip to next cycle
		}
		if len(routers) == 0 {
			fmt.Println("No routers configured. Exiting this cycle, will retry in 30 minutes.")
			time.Sleep(30 * time.Minute)
			continue // Skip to next cycle
		}

		// 2. Connect to databases
		connStats, err := connectDB(STATS_DB_NAME)
		if err != nil {
			fmt.Printf("Failed to connect to stats database: %v\n", err)
			time.Sleep(30 * time.Minute)
			continue // Skip to next cycle
		}
		defer connStats.Close() // Ensure stats DB connection is closed

		connDHCP, err := connectDB(DHCP_DB_NAME)
		if err != nil {
			fmt.Printf("Failed to connect to DHCP database: %v\n", err)
			time.Sleep(30 * time.Minute)
			continue // Skip to next cycle
		}
		defer connDHCP.Close() // Ensure DHCP DB connection is closed

		// Mutex to protect database writes from concurrent access
		var dbMutex sync.Mutex

		// 3. Setup database tables
		if err := setupStatsDB(connStats); err != nil {
			fmt.Printf("Failed to set up stats database: %v\n", err)
			time.Sleep(30 * time.Minute)
			continue // Skip to next cycle
		}
		if err := setupDHCPDB(connDHCP); err != nil {
			fmt.Printf("Failed to set up DHCP database: %v\n", err)
			time.Sleep(30 * time.Minute)
			continue // Skip to next cycle
		}

		// 4. Reset monthly stats if needed
		if err := resetMonthlyStats(connStats, &dbMutex); err != nil {
			fmt.Printf("Failed to reset monthly stats: %v\n", err)
			// Continue execution even if reset fails, as it's not critical for data collection
		}

		// Use a wait group to wait for all goroutines to complete
		// This allows concurrent processing of multiple routers
		var wg sync.WaitGroup

		for routerIP, urls := range routers {
			wg.Add(1) // Increment the counter for each goroutine
			go func(routerIP string, urls RouterConfig) {
				defer wg.Done() // Decrement the counter when the goroutine finishes

				fmt.Printf("Processing router: %s\n", routerIP)

				// Fetch and parse AP stats (WiFi clients)
				apData, err := fetchData(urls.APStatsURL)
				if err != nil {
					if err != ErrURLEmpty { // Only print error if it's not due to an empty URL
						fmt.Printf("Error fetching AP stats for %s: %v\n", routerIP, err)
					}
				} else {
					clients, err := parseWiFiStats(apData)
					if err != nil {
						fmt.Printf("Error parsing WiFi stats for %s: %v\n", routerIP, err)
					} else if len(clients) > 0 {
						for _, client := range clients {
							if err := updateTrafficStats(connStats, &dbMutex, client.MACAddress, client.RXBytes, client.TXBytes); err != nil {
								fmt.Printf("Error updating traffic stats for client %s (%s): %v\n", client.MACAddress, routerIP, err)
							}
						}
					} else {
						fmt.Printf("No WiFi client data found for %s.\n", routerIP)
					}
				}

				// Fetch and parse WAN stats
				wanData, err := fetchData(urls.WANStatsURL)
				if err != nil {
					if err != ErrURLEmpty { // Only print error if it's not due to an empty URL
						fmt.Printf("Error fetching WAN stats for %s: %v\n", routerIP, err)
					}
				} else {
					wan, err := parseWANStats(wanData)
					if err != nil {
						fmt.Printf("Error parsing WAN stats for %s: %v\n", routerIP, err)
					} else if wan != nil {
						if err := updateTrafficStats(connStats, &dbMutex, "main_wan", wan.RXBytes, wan.TXBytes); err != nil {
							fmt.Printf("Error updating traffic stats for main_wan (%s): %v\n", routerIP, err)
						}
					} else {
						fmt.Printf("No WAN data found for %s.\n", routerIP)
					}
				}

				// Fetch and parse DHCP leases
				dhcpData, err := fetchData(urls.DHCPLeasesURL)
				if err != nil {
					if err != ErrURLEmpty { // Only print error if it's not due to an empty URL
						fmt.Printf("Error fetching DHCP leases for %s: %v\n", routerIP, err)
					}
				} else {
					leases, err := parseDHCPLeases(dhcpData)
					if err != nil {
						fmt.Printf("Error parsing DHCP leases for %s: %v\n", routerIP, err)
					} else if len(leases) > 0 {
						if err := upsertDHCPLeases(connDHCP, &dbMutex, leases); err != nil {
							fmt.Printf("Error upserting DHCP leases for %s: %v\n", routerIP, err)
						}
					} else {
						fmt.Printf("No DHCP lease data found for %s.\n", routerIP)
					}
				}
			}(routerIP, urls) // Pass loop variables as arguments to the goroutine
		}

		wg.Wait() // Wait for all goroutines to finish
		fmt.Println("Data collection cycle complete. Sleeping for 30 minutes...")
		time.Sleep(30 * time.Minute) // Wait for 30 minutes before the next cycle
	}
}
