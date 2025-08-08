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

* **PHP API for Data Retrieval:** Includes a companion PHP script (`api.php`) to easily fetch collected data as JSON for web visualization or other uses.

---

## Setup and Usage

### Prerequisites

* **Go Language:** Go 1.16 or newer installed on your Orange Pi Zero 3.

  * If Go is not installed: `sudo apt update && sudo apt install golang`

* **C Compiler (GCC):** Required for compiling the SQLite3 Go driver.

  * If GCC is not installed: `sudo apt update && sudo apt install build-essential`

* **PHP with SQLite3 Extension:** Required if you plan to use the `api.php` script.

  * If PHP-FPM is not installed: `sudo apt update && sudo apt install php8.2-fpm` (adjust `8.2` to your PHP version).

  * If SQLite3 extension is not installed: `sudo apt install php8.2-sqlite3` (adjust `8.2` to your PHP version).

* **Nginx/Apache:** A web server to serve the PHP API.

* **Router Endpoints:** Your router must expose endpoints for `totalwifi.cgi` (WiFi client stats), `wan.cgi` (WAN interface stats), and `dhcp.cgi` (DHCP leases). These are common on OpenWrt-based systems.

### 1. Application Files

Place the following files in a dedicated directory on your Orange Pi Zero 3, for example, `/home/wan/netstat/`:

* `main.go`: The Go source code for the application.

* `routers.json`: The configuration file specifying your router(s) and their respective URLs.

**Example `routers.json`:**

```
{
    "192.168.1.1": {
        "ap_stats": "http://192.168.1.1/cgi-bin/totalwifi.cgi",
        "wan_stats": "http://192.168.1.1/cgi-bin/wan.cgi",
        "dhcp_leases": "http://192.168.1.1/cgi-bin/dhcp.cgi"
    },
    "192.168.1.2": {
        "ap_stats": "http://192.168.1.2/cgi-bin/totalwifi.cgi",
        "wan_stats": "",       // The script will skip fetching for empty URLs
        "dhcp_leases": ""
    }
}


```

* **Important:** Ensure the URLs in `routers.json` are correct for your router. If a URL is empty, the script will gracefully skip fetching data for that endpoint.

### 2. Compile the Go Application (on Orange Pi Zero 3)

Navigate to your project directory (e.g., `/home/wan/netstat/`) and compile the Go application:

```
cd /home/wan/netstat/

# Initialize Go module (only once per project)
go mod init router_stats

# Download the SQLite driver dependency
go get [github.com/mattn/go-sqlite3](https://github.com/mattn/go-sqlite3)

# IMPORTANT: If you get "build output 'router_stats_go' already exists" error, remove it first:
rm router_stats_go

# Build the executable
go build -o router_stats_go

# Make the executable runnable
chmod +x router_stats_go


```

This will create an executable file named `router_stats_go` in your `/home/wan/netstat/` directory.

### 3. Database Location and Permissions

The Go script is configured to store database files in `/var/www/netstat-data/`. This location is generally more appropriate for data accessed by web services.

1. **Create the database directory:**

   ```
   sudo mkdir -p /var/www/netstat-data/
   
   
   ```

2. **Set appropriate permissions for both Go script (running as `wan`) and PHP-FPM (running as `www-data`):**

   ```
   # Ensure the directory and its contents are owned by 'wan' user and 'wan' group initially
   sudo chown -R wan:wan /var/www/netstat-data/
   
   # Add the 'www-data' user to the 'wan' group
   sudo usermod -a -G wan www-data
   
   # Set permissions for the directory:
   # Owner (wan): rwx
   # Group (wan, which includes www-data): rwx
   # Others: r-x
   sudo chmod 775 /var/www/netstat-data/
   
   # Set permissions for the database files (once created by the Go script):
   # Owner (wan): rw
   # Group (wan, which includes www-data): rw
   # Others: r
   sudo chmod 664 /var/www/netstat-data/*.db
   
   
   ```

   * **Note:** The `.db` files will be created by the `router_stats_go` script on its first run. You can run `sudo chmod 664 /var/www/netstat-data/*.db` again after the first run to ensure permissions are applied.

### 4. Run as a Systemd Service (Recommended for Continuous Operation)

To ensure the Go script runs continuously in the background and starts automatically on boot, it's recommended to run it as a `systemd` service.

1. **Create the Systemd Service Unit File:**

   ```
   sudo nano /etc/systemd/system/router-stats.service
   
   
   ```

   Paste the following content:

   ```
   [Unit]
   Description=Router Statistics Collector (Continuous)
   After=network.target
   
   [Service]
   Type=simple
   User=wan
   Group=wan
   WorkingDirectory=/home/wan/netstat/
   ExecStart=/home/wan/netstat/router_stats_go
   Restart=on-failure
   RestartSec=5s
   StandardOutput=journal
   StandardError=journal
   
   [Install]
   WantedBy=multi-user.target
   
   
   ```

   Save and close the file.

2. **Reload Systemd and Enable the Service:**

   ```
   sudo systemctl daemon-reload
   sudo systemctl enable router-stats.service
   
   
   ```

3. **Start the Service:**

   ```
   sudo systemctl start router-stats.service
   
   
   ```

### 5. Setup PHP API (Optional, for Web Access)

If you want to expose the data via a web API, use the provided `api.php` script.

1. **Place `api.php`:** Copy `api.php` to your web server's document root (e.g., `/var/www/html/netstat/`):

   ```
   sudo mkdir -p /var/www/html/netstat/
   sudo cp /path/to/your/api.php /var/www/html/netstat/
   
   
   ```

2. **Update Database Paths in `api.php`:** Ensure the `$statsDbPath` and `$dhcpDbPath` variables in `api.php` point to `/var/www/netstat-data/`.

   ```
   $statsDbPath = '/var/www/netstat-data/network_stats.db';
   $dhcpDbPath = '/var/www/netstat-data/dhcp_leases.db';
   
   
   ```

3. **Configure Web Server (Nginx Example):**
   Ensure your Nginx server block (`/etc/nginx/sites-available/default` or custom config) is set up to process PHP files via PHP-FPM. Confirm the `fastcgi_pass` directive matches your PHP-FPM socket (e.g., `unix:/run/php/php8.2-fpm.sock`).

   ```
   location ~ \.php$ {
       include snippets/fastcgi-php.conf;
       fastcgi_pass unix:/run/php/php8.2-fpm.sock; # Adjust version if needed
       fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
       include fastcgi_params;
   }
   
   
   ```

4. **Restart PHP-FPM and Nginx:**

   ```
   sudo systemctl restart php8.2-fpm # Adjust version
   sudo systemctl restart nginx
   
   
   ```

5. **Access the API:**

   * `http://your-server-ip/netstat/api.php?action=clients` (monthly client traffic)

   * `http://your-server-ip/netstat/api.php?action=wan` (monthly WAN traffic)

   * `http://your-server-ip/netstat/api.php?action=leases` (all DHCP lease data)

   * `http://your-server-ip/netstat/api.php?action=combined` (single JSON object with both monthly client and WAN traffic)

## Database Output

The script will create two SQLite database files in `/var/www/netstat-data/`:

1. **`network_stats.db`**

   * `cumulative_stats` table: Stores the last known total RX/TX bytes for each entity (MAC address or "main_wan").

   * `monthly_stats` table: Stores the aggregated monthly RX/TX bytes for each entity. These totals are reset to `0` at the beginning of each new calendar month.

2. **`dhcp_leases.db`**

   * `dhcp_leases` table: Stores details about active DHCP leases.

You can use the `sqlite3` command-line tool on your Orange Pi Zero 3 or a graphical SQLite browser on your desktop to view the data in these files.
