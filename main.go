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
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type RouterConfig struct {
	APStatsURL    string `json:"ap_stats"`
	WANStatsURL   string `json:"wan_stats"`
	DHCPLeasesURL string `json:"dhcp_leases"`
}

type Config map[string]RouterConfig

const (
	STATS_DB_NAME = "/var/www/netstat-data/network_stats.db"
	DHCP_DB_NAME  = "/var/www/netstat-data/dhcp_leases.db"
	CONFIG_FILE   = "routers.json"
)

type ClientStats struct {
	MACAddress string
	RXBytes    int64
	TXBytes    int64 // Corrected: Changed from 64 to int64
}

type WANStats struct {
	RXBytes int64
	TXBytes int64
}

type DHCPLease struct {
	MACAddress   string
	LeaseEndTime int64
	IPAddress    string
	Hostname     string
	ClientID     string
}

var ErrURLEmpty = fmt.Errorf("URL is empty")

func loadConfig(filename string) (Config, error) {
	file, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("error: Configuration file '%s' not found", filename)
		}
		return nil, fmt.Errorf("error opening config file '%s': %w", filename, err)
	}
	defer file.Close()

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

func connectDB(dbName string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbName)
	if err != nil {
		return nil, fmt.Errorf("database connection error for %s: %w", dbName, err)
	}
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("database ping error for %s: %w", dbName, err)
	}
	return db, nil
}

func setupStatsDB(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction for stats DB setup: %w", err)
	}
	defer tx.Rollback()

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

	return tx.Commit()
}

func setupDHCPDB(db *sql.DB) error {
	tx, err := db.Begin()
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

func resetMonthlyStats(db *sql.DB, mutex *sync.Mutex) error {
	mutex.Lock()
	defer mutex.Unlock()

	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM monthly_stats").Scan(&count)
	if err != nil {
		return fmt.Errorf("error checking monthly_stats table count: %w", err)
	}
	if count == 0 {
		return nil
	}

	var lastUpdateStr string
	err = db.QueryRow("SELECT timestamp FROM monthly_stats ORDER BY timestamp DESC LIMIT 1").Scan(&lastUpdateStr)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("error fetching last update timestamp from monthly_stats: %w", lastUpdateStr, err)
	}

	lastUpdateDate, err := time.Parse("2006-01-02 15:04:05", lastUpdateStr)
	if err != nil {
		return fmt.Errorf("error parsing last update timestamp '%s': %w", lastUpdateStr, err)
	}

	currentDate := time.Now()

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

func fetchData(url string) (string, error) {
	if url == "" {
		return "", ErrURLEmpty
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}

	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("error fetching data from %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP error fetching data from %s: %d - %s", url, resp.StatusCode, resp.Status)
	}

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response body from %s: %w", url, err)
	}

	return string(bodyBytes), nil
}

func parseWiFiStats(data string) ([]ClientStats, error) {
	if data == "" {
		return nil, nil
	}

	var clients []ClientStats
	lines := strings.Split(strings.TrimSpace(data), "\n")
	for _, line := range lines {
		parts := strings.Fields(line)
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

func parseWANStats(data string) (*WANStats, error) {
	if data == "" {
		return nil, nil
	}

	re := regexp.MustCompile(`wan:\s+(\d+)\s+(\d+)`)
	match := re.FindStringSubmatch(data)

	if len(match) == 3 {
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

func parseDHCPLeases(data string) ([]DHCPLease, error) {
	if data == "" {
		return nil, nil
	}

	var leases []DHCPLease
	lines := strings.Split(strings.TrimSpace(data), "\n")
	ipv4LeasePattern := regexp.MustCompile(
		`^(\d+)\s+([0-9a-fA-F:]{17})\s+([\d\.]+)\s+(.*?)\s+([\d0-9a-fA-F:]+)$`,
	)

	for _, line := range lines {
		match := ipv4LeasePattern.FindStringSubmatch(line)
		if len(match) == 6 {
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

func updateTrafficStats(db *sql.DB, mutex *sync.Mutex, entityID string, newRX, newTX int64) error {
	mutex.Lock()
	defer mutex.Unlock()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction for traffic stats: %w", err)
	}
	defer tx.Rollback()

	var lastRX, lastTX int64
	err = tx.QueryRow("SELECT rx_bytes, tx_bytes FROM cumulative_stats WHERE id = ?", entityID).Scan(&lastRX, &lastTX)

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
		incrementalRX = newRX
		incrementalTX = newTX
	} else if err != nil {
		return fmt.Errorf("error fetching cumulative stats for %s: %w", entityID, err)
	} else {
		if newRX >= lastRX {
			incrementalRX = newRX - lastRX
		} else {
			incrementalRX = newRX
		}

		if newTX >= lastTX {
			incrementalTX = newTX - lastTX
		} else {
			incrementalTX = newTX
		}
	}

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

	_, err = tx.Exec(`
		INSERT OR REPLACE INTO cumulative_stats (id, rx_bytes, tx_bytes)
		VALUES (?, ?, ?)
	`, entityID, newRX, newTX)
	if err != nil {
		return fmt.Errorf("error upserting cumulative stats for %s: %w", entityID, err)
	}

	return tx.Commit()
}

func upsertDHCPLeases(db *sql.DB, mutex *sync.Mutex, leases []DHCPLease) error {
	if len(leases) == 0 {
		return nil
	}

	mutex.Lock()
	defer mutex.Unlock()

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

func main() {
	for {
		fmt.Println("Starting data collection cycle...")
		routers, err := loadConfig(CONFIG_FILE)
		if err != nil {
			fmt.Printf("Failed to load configuration: %v\n", err)
			time.Sleep(30 * time.Minute)
			continue
		}
		if len(routers) == 0 {
			fmt.Println("No routers configured. Exiting this cycle, will retry in 30 minutes.")
			time.Sleep(30 * time.Minute)
			continue
		}

		connStats, err := connectDB(STATS_DB_NAME)
		if err != nil {
			fmt.Printf("Failed to connect to stats database: %v\n", err)
			time.Sleep(30 * time.Minute)
			continue
		}
		defer connStats.Close()

		connDHCP, err := connectDB(DHCP_DB_NAME)
		if err != nil {
			fmt.Printf("Failed to connect to DHCP database: %v\n", err)
			time.Sleep(30 * time.Minute)
			continue
		}
		defer connDHCP.Close()

		var dbMutex sync.Mutex

		if err := setupStatsDB(connStats); err != nil {
			fmt.Printf("Failed to set up stats database: %v\n", err)
			time.Sleep(30 * time.Minute)
			continue
		}
		if err := setupDHCPDB(connDHCP); err != nil {
			fmt.Printf("Failed to set up DHCP database: %v\n", err)
			time.Sleep(30 * time.Minute)
			continue
		}

		if err := resetMonthlyStats(connStats, &dbMutex); err != nil {
			fmt.Printf("Failed to reset monthly stats: %v\n", err)
		}

		var wg sync.WaitGroup

		for routerIP, urls := range routers {
			wg.Add(1)
			go func(routerIP string, urls RouterConfig) {
				defer wg.Done()

				fmt.Printf("Processing router: %s\n", routerIP)

				apData, err := fetchData(urls.APStatsURL)
				if err != nil {
					if err != ErrURLEmpty {
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

				wanData, err := fetchData(urls.WANStatsURL)
				if err != nil {
					if err != ErrURLEmpty {
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

				dhcpData, err := fetchData(urls.DHCPLeasesURL)
				if err != nil {
					if err != ErrURLEmpty {
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
			}(routerIP, urls)
		}

		wg.Wait()
		fmt.Println("Data collection cycle complete. Sleeping for 30 minutes...")
		time.Sleep(30 * time.Minute)
	}
}
