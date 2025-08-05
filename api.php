<?php
/**
 * PHP API to fetch network statistics from SQLite databases.
 *
 * This script connects to the `network_stats.db` and `dhcp_leases.db`
 * databases created by the Python script and serves the data as JSON.
 *
 * This version of the API fetches pre-calculated monthly totals from
 * the 'monthly_stats' table.
 *
 * Usage:
 * - http://your-server-ip/api.php?action=clients  (gets monthly client traffic data)
 * - http://your-server-ip/api.php?action=wan      (gets monthly WAN traffic data)
 * - http://your-server-ip/api.php?action=leases   (gets all DHCP lease data)
 * - http://your-server-ip/api.php?action=combined (gets a single JSON object with both monthly client and WAN traffic)
 */

header('Content-Type: application/json');
date_default_timezone_set('Asia/Kuala_Lumpur'); // Set your timezone

// --- Database Configuration ---
// Adjust the path to your database files.
$statsDbPath = '/var/www/netstat-data/network_stats.db';
$dhcpDbPath = '/var/www/netstat-data/dhcp_leases.db';

// --- Functions ---

/**
 * Establishes a read-only connection to an SQLite database.
 * @param string $dbPath The path to the SQLite database file.
 * @return SQLite3|false An SQLite3 object on success, or false on failure.
 */
function connectDb($dbPath) {
    try {
        if (!file_exists($dbPath)) {
            // Log this error for debugging
            error_log("Database file not found at: " . $dbPath);
            return false;
        }
        $db = new SQLite3($dbPath, SQLITE3_OPEN_READONLY);
        $db->busyTimeout(5000); // Set a timeout to handle locked databases
        return $db;
    } catch (Exception $e) {
        error_log("Failed to connect to database: " . $e->getMessage());
        return false;
    }
}

/**
 * Fetches all data from a specified table.
 * @param SQLite3 $db The database connection object.
 * @param string $tableName The name of the table to query.
 * @return array An array of results.
 */
function fetchAllData($db, $tableName) {
    $data = [];
    try {
        $results = $db->query("SELECT * FROM $tableName");
        if ($results) {
            while ($row = $results->fetchArray(SQLITE3_ASSOC)) {
                $data[] = $row;
            }
        }
    } catch (Exception $e) {
        error_log("Error fetching data from table {$tableName}: " . $e->getMessage());
    }
    return $data;
}

// --- API Endpoint Logic ---
if (!isset($_GET['action'])) {
    http_response_code(400); // Bad Request
    echo json_encode(['error' => 'No action specified.']);
    exit();
}

$action = $_GET['action'];

try {
    switch ($action) {
        case 'clients':
            $db = connectDb($statsDbPath);
            if (!$db) {
                http_response_code(500);
                echo json_encode(['error' => 'Could not connect to the stats database.']);
                exit();
            }
            $results = $db->query("SELECT id, rx_bytes, tx_bytes FROM monthly_stats WHERE id != 'main_wan'");
            $data = [];
            while ($row = $results->fetchArray(SQLITE3_ASSOC)) {
                $data[] = $row;
            }
            echo json_encode(['data' => $data]);
            $db->close();
            break;
            
        case 'wan':
            $db = connectDb($statsDbPath);
            if (!$db) {
                http_response_code(500);
                echo json_encode(['error' => 'Could not connect to the stats database.']);
                exit();
            }
            $results = $db->query("SELECT id, rx_bytes, tx_bytes, timestamp FROM monthly_stats WHERE id = 'main_wan'");
            $data = [];
            if ($row = $results->fetchArray(SQLITE3_ASSOC)) {
                 $dateTime = new DateTime($row['timestamp']);
                 $row['last_update'] = $dateTime->format('Y-m-d H:i:s');
                 unset($row['timestamp']);
                 $data[] = $row;
            }
            echo json_encode(['data' => $data]);
            $db->close();
            break;

        case 'leases':
            $db = connectDb($dhcpDbPath);
            if (!$db) {
                http_response_code(500);
                echo json_encode(['error' => 'Could not connect to the DHCP database.']);
                exit();
            }
            $data = fetchAllData($db, 'dhcp_leases');
            echo json_encode(['data' => $data]);
            $db->close();
            break;
        
        case 'combined':
            $statsDb = connectDb($statsDbPath);
            $leasesDb = connectDb($dhcpDbPath);
            if (!$statsDb || !$leasesDb) {
                http_response_code(500);
                echo json_encode(['error' => 'Could not connect to one or more databases.']);
                exit();
            }

            // Fetch the pre-calculated monthly totals from the 'monthly_stats' table
            $monthlyStats = fetchAllData($statsDb, 'monthly_stats');
            $dhcpLeases = fetchAllData($leasesDb, 'dhcp_leases');
            
            // Map DHCP leases by MAC address for quick lookup
            $leasesByMac = [];
            foreach ($dhcpLeases as $lease) {
                $leasesByMac[$lease['mac_address']] = $lease;
            }

            $combinedClientStats = [];
            $wanStats = [
                'rx_bytes' => 0,
                'tx_bytes' => 0,
                'last_update' => null
            ];

            foreach ($monthlyStats as $stat) {
                $entityId = $stat['id'];
                
                // Separate WAN stats from client stats
                if ($entityId === 'main_wan') {
                    $dateTime = new DateTime($stat['timestamp']);
                    $wanStats = [
                        'rx_bytes' => $stat['rx_bytes'],
                        'tx_bytes' => $stat['tx_bytes'],
                        'last_update' => $dateTime->format('Y-m-d H:i:s')
                    ];
                } else {
                    $mac = $entityId;
                    $hostname = 'Unknown';
                    
                    // Look up hostname from DHCP leases
                    if (isset($leasesByMac[$mac])) {
                        $lease = $leasesByMac[$mac];
                        if (!empty($lease['hostname']) && $lease['hostname'] !== 'Unknown') {
                            $hostname = $lease['hostname'];
                        } else {
                            $hostname = $lease['ip_address'];
                        }
                    } else {
                        $hostname = $mac;
                    }
                    
                    $combinedClientStats[] = [
                        'rx_bytes' => $stat['rx_bytes'],
                        'tx_bytes' => $stat['tx_bytes'],
                        'hostname' => $hostname
                    ];
                }
            }

            echo json_encode([
                'wan_stats' => $wanStats,
                'client_stats' => $combinedClientStats
            ]);
            
            $statsDb->close();
            $leasesDb->close();
            break;

        default:
            http_response_code(400); // Bad Request
            echo json_encode(['error' => 'Invalid action. Valid actions are: clients, wan, leases, combined.']);
            break;
    }
} catch (Exception $e) {
    http_response_code(500); // Internal Server Error
    echo json_encode(['error' => 'An unexpected error occurred: ' . $e->getMessage()]);
}

?>
