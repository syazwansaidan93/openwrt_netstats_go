# Router Statistics Collector (Go)

This Go application is designed to periodically collect network traffic statistics from your router (specifically, devices running OpenWrt or similar firmware that expose `cgi-bin` endpoints for stats and DHCP leases) and store them in SQLite databases. It tracks cumulative and monthly traffic for connected clients and the main WAN interface, as well as current DHCP lease information.

## Features

* **Traffic Monitoring:** Collects RX (received) and TX (transmitted) bytes for WiFi clients and the main WAN interface.
* **Monthly Aggregation:** Aggregates traffic data on a monthly basis, resetting totals at the start of each new month.
* **Router Reset Handling:** Intelligently handles router reboots by detecting decreases in cumulative byte counters and adjusting incremental calculations.
* **DHCP Lease Tracking:** Records DHCP lease details including MAC address, IP address, hostname, and lease expiration time.
* **Concurrent Processing:** Uses Go goroutines to fetch data from multiple routers concurrently.
* **SQLite Storage:** Stores all data in local SQLite database files (`network_stats.db` and `dhcp_leases.db`).
* **Internal Scheduling:** The application runs in a continuous loop, performing data collection every 30 minutes.

## Setup and Usage

### Prerequisites

* **Go Language:** Go 1.16 or newer installed on your Orange Pi Zero 3.
* **Router Endpoints:** Your router must expose endpoints for `totalwifi.cgi` (WiFi client stats), `wan.cgi` (WAN interface stats), and `dhcp.cgi` (DHCP leases). These are common on OpenWrt-based systems.

### 1. Application Files

Place the following files in a dedicated directory on your Orange Pi Zero 3, for example, `/home/wan/netstat/`:

* `main.go`: The Go source code for the application.
* `routers.json`: The configuration file specifying your router(s) and their respective URLs.

**Example `routers.json`:**

```json
{
    "192.168.1.1": {
        "ap_stats": "[http://192.168.1.1/cgi-bin/totalwifi.cgi](http://192.168.1.1/cgi-bin/totalwifi.cgi)",
        "wan_stats": "[http://192.168.1.1/cgi-bin/wan.cgi](http://192.168.1.1/cgi-bin/wan.cgi)",
        "dhcp_leases": "[http://192.168.1.1/cgi-bin/dhcp.cgi](http://192.168.1.1/cgi-bin/dhcp.cgi)"
    },
    "192.168.1.2": {
        "ap_stats": "",        // Leave empty if not available or not needed
        "wan_stats": "",       // The script will skip fetching for empty URLs
        "dhcp_leases": ""
    }
}
```
* **Important:** Ensure the URLs in `routers.json` are correct for your router. If a URL is empty, the script will gracefully skip fetching data for that endpoint without logging an error.

### 2. Compile the Application (on Orange Pi Zero 3)

Navigate to your project directory (e.g., `/home/wan/netstat/`) and compile the Go application:

```bash
cd /home/wan/netstat/

# Initialize Go module (only once per project)
go mod init router_stats

# Download the SQLite driver dependency
go get [github.com/mattn/go-sqlite3](https://github.com/mattn/go-sqlite3)

# Build the executable
go build -o router_stats_go

# Make the executable runnable
chmod +x router_stats_go
```
This will create an executable file named `router_stats_go` in your `/home/wan/netstat/` directory.

### 3. Run as a Systemd Service (Recommended)

To ensure the script runs continuously in the background and starts automatically on boot, it's recommended to run it as a `systemd` service.

1.  **Create the Systemd Service Unit File:**
    ```bash
    sudo nano /etc/systemd/system/router-stats.service
    ```
    Paste the following content:
    ```ini
    [Unit]
    Description=Router Statistics Collector (Continuous)
    After=network.target

    [Service]
    Type=simple              # Service type is simple as the Go program runs continuously
    User=wan                 # Run the service as your user
    Group=wan                # Run the service under your user's group
    WorkingDirectory=/home/wan/netstat/ # Set the working directory to your script's location
    ExecStart=/home/wan/netstat/router_stats_go # The full path to your compiled executable
    Restart=on-failure       # Restart the service if it crashes
    RestartSec=5s            # Wait 5 seconds before restarting
    StandardOutput=journal   # Log standard output to journalctl
    StandardError=journal    # Log standard error to journalctl

    [Install]
    WantedBy=multi-user.target
    ```
    Save and close the file.

2.  **Reload Systemd and Enable the Service:**
    ```bash
    sudo systemctl daemon-reload
    sudo systemctl enable router-stats.service
    ```

3.  **Start the Service:**
    ```bash
    sudo systemctl start router-stats.service
    ```

### 4. Verify Operation

* **Check Service Status:**
    ```bash
    sudo systemctl status router-stats.service
    ```
* **View Logs (real-time):**
    ```bash
    journalctl -u router-stats.service -f
    ```
    You should see messages indicating "Starting data collection cycle..." and "Data collection cycle complete. Sleeping for 30 minutes..." every 30 minutes.

## Database Output

The script will create two SQLite database files in the `WorkingDirectory` (`/home/wan/netstat/`):

1.  **`network_stats.db`**
    * **`cumulative_stats` table:** Stores the last known total RX/TX bytes for each entity (MAC address or "main_wan"). Used for calculating incremental traffic and handling router resets.
        * `id` (TEXT PRIMARY KEY)
        * `rx_bytes` (INTEGER)
        * `tx_bytes` (INTEGER)
    * **`monthly_stats` table:** Stores the aggregated monthly RX/TX bytes for each entity. These totals are reset to `0` at the beginning of each new calendar month.
        * `id` (TEXT PRIMARY KEY)
        * `rx_bytes` (INTEGER)
        * `tx_bytes` (INTEGER)
        * `timestamp` (TEXT)

2.  **`dhcp_leases.db`**
    * **`dhcp_leases` table:** Stores details about active DHCP leases.
        * `mac_address` (TEXT PRIMARY KEY)
        * `lease_end_time` (INTEGER)
        * `ip_address` (TEXT)
        * `hostname` (TEXT)
        * `client_id` (TEXT)
        * `timestamp` (TEXT)

You can use the `sqlite3` command-line tool or a graphical SQLite browser to view the data in these files.
