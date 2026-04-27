package main

import (
	_ "embed"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/breml/rootcerts" // embed Mozilla CA bundle as fallback for scratch containers

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/oschwald/maxminddb-golang"
)

//go:embed static/index.html
var indexHTML []byte

//go:embed static/favicon.png
var faviconPNG []byte

//go:embed static/openapi.json
var openapiJSON []byte

/* ---------- IPv6 MMDB keyspace ---------- */

// ipv6Prefix is the fixed 10-byte ULA prefix for the synthetic MAC→IPv6 keyspace.
//
// The full 16-byte IPv6 MMDB key is:
//
//	bytes  0–9  : fd:ac:db:00:00:00:00:00:00:00   (this prefix, 80 bits)
//	bytes 10–15 : 6 bytes of the MAC address (address_min padded with trailing zeros)
//
// The CIDR mask width is (80 + prefixBits). Example for MA-L OUI "00:00:05" (24 bits):
//
//	key → fdac:db00:0000:0000:0000:0000:0500:0000/104
//
// fdac:db00::/32 is within the ULA range (fc00::/7) and never routes publicly,
// so collisions with real IP addresses are impossible.
var ipv6Prefix = [10]byte{0xfd, 0xac, 0xdb, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}

/* ---------- Registered assignments ---------- */

// registeredAssignments is the set of IEEE assignment types that indicate
// a publicly registered (globally administered) MAC block.
var registeredAssignments = map[string]bool{
	"MA-L": true,
	"MA-M": true,
	"MA-S": true,
	"CID":  true,
	"IAB":  true,
}

/* ---------- mmdb record type ---------- */

// MACRecord is the data stored per MAC prefix block in the mmdb database.
// Fields use maxminddb tags that match the keys written by mmdbwriter.
type MACRecord struct {
	OUI         string `maxminddb:"oui"`
	OrgName     string `maxminddb:"organisation_name"`
	OrgAddress  string `maxminddb:"organization_address"`
	CountryCode string `maxminddb:"country_code"`
	AddressMin  string `maxminddb:"address_min"`
	AddressMax  string `maxminddb:"address_max"`
	BlockSize   uint64 `maxminddb:"block_size"`
	Assignment  string `maxminddb:"assignment"`
	Virtual     string `maxminddb:"virtual"` // "False" or virtualisation technology name
}

/* ---------- API types ---------- */

// MACAnswer is one result item in the API response.
type MACAnswer struct {
	MAC         string  `json:"mac"`
	Valid       bool    `json:"valid"`
	MACType     string  `json:"type"`
	AdminType   string  `json:"admin_type,omitempty"`
	Registered  bool    `json:"registered"`
	OUI         string  `json:"oui,omitempty"`
	OrgName     string  `json:"organisation_name,omitempty"`
	OrgAddress  string  `json:"organization_address,omitempty"`
	CountryCode string  `json:"country_code,omitempty"`
	AddressMin  string  `json:"address_min,omitempty"`
	AddressMax  string  `json:"address_max,omitempty"`
	BlockSize   *uint64 `json:"block_size,omitempty"`
	Assignment  string  `json:"assignment,omitempty"`
	Virtual     string  `json:"virtual,omitempty"`
}

// MACResponse is the JSON body returned by the API.
type MACResponse struct {
	Status  string      `json:"status"`
	Answers []MACAnswer `json:"answers"`
}

// MACRequest is the JSON body expected by the API.
type MACRequest struct {
	MAC  *string  `json:"mac"`
	MACs []string `json:"macs"`
}

/* ---------- Configuration ---------- */

var (
	maxMACs    int
	dbDir      string
	dbURL      string // base URL of a peer instance; empty = build from CDN CSV
	licenseKey string // LICENSE_KEY token for the CDN; may be empty (anonymous)
	listenAddr string
	dbValue    atomic.Value // (*maxminddb.Reader)
)

const (
	lastUpdateFile   = ".last_update_mac"
	lastModifiedFile = ".last_modified_mac"
	dbFileName       = "mac.mmdb"
	cdnCSVURL        = "https://cdn.letstool.net/mac/csv"
	updateInterval   = 24 * time.Hour
)

/* ---------- MAC Validation & Normalization ---------- */

// normalizeMAC parses a MAC address in any of the supported formats and returns
// the normalized 6-byte hardware address and a validity flag.
//
// Accepted formats:
//
//	xx:xx:xx:xx:xx:xx  (Linux / IEEE colon-separated)
//	xx-xx-xx-xx-xx-xx  (Windows hyphen-separated)
//	xx.xx.xx.xx.xx.xx  (dot-separated per-byte)
//	xxxx.xxxx.xxxx     (Cisco three-group format)
//	xxxxxxxxxxxx       (no separator, 12 hex digits)
func normalizeMAC(s string) (net.HardwareAddr, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}

	var hexStr string
	colonCount := strings.Count(s, ":")
	hyphenCount := strings.Count(s, "-")
	dotCount := strings.Count(s, ".")

	switch {
	case colonCount == 5:
		// xx:xx:xx:xx:xx:xx
		hexStr = strings.ReplaceAll(s, ":", "")
	case hyphenCount == 5:
		// xx-xx-xx-xx-xx-xx
		hexStr = strings.ReplaceAll(s, "-", "")
	case dotCount == 5:
		// xx.xx.xx.xx.xx.xx
		hexStr = strings.ReplaceAll(s, ".", "")
	case dotCount == 2:
		// xxxx.xxxx.xxxx (Cisco)
		hexStr = strings.ReplaceAll(s, ".", "")
	case colonCount == 0 && hyphenCount == 0 && dotCount == 0:
		// xxxxxxxxxxxx (no separator)
		hexStr = s
	default:
		return nil, false
	}

	if len(hexStr) != 12 {
		return nil, false
	}

	b, err := hex.DecodeString(hexStr)
	if err != nil || len(b) != 6 {
		return nil, false
	}

	return net.HardwareAddr(b), true
}

// macType returns "Multicast" if the LSB of the first octet is set (multicast/broadcast bit),
// otherwise "Unicast". The MAC address must already be validated.
func macAddressType(mac net.HardwareAddr) string {
	if mac[0]&0x01 != 0 {
		return "Multicast"
	}
	return "Unicast"
}

// macAdminType returns "LAA" (Locally Administered Address) if the U/L bit
// (bit 1, second-least-significant bit of the first octet) is set,
// otherwise "UAA" (Universally Administered Address).
// UAA addresses are globally unique and factory-assigned by the manufacturer.
// LAA addresses are locally/manually assigned and may be random or virtual.
func macAdminType(mac net.HardwareAddr) string {
	if mac[0]&0x02 != 0 {
		return "LAA"
	}
	return "UAA"
}

// macToIPv6 converts a 6-byte MAC address to the synthetic IPv6 address
// used as the MMDB lookup key.
func macToIPv6(mac net.HardwareAddr) net.IP {
	ip := make(net.IP, 16)
	copy(ip[0:10], ipv6Prefix[:])
	copy(ip[10:16], mac)
	return ip
}

// formatMAC returns a canonical lowercase colon-separated MAC string.
func formatMAC(mac net.HardwareAddr) string {
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		mac[0], mac[1], mac[2], mac[3], mac[4], mac[5])
}

// parseMACBytes strips separators from a MAC string (any format) and returns
// the 6 raw bytes, or an error.
func parseMACBytes(s string) ([6]byte, error) {
	s = strings.ReplaceAll(s, ":", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, ".", "")
	s = strings.TrimSpace(s)
	if len(s) != 12 {
		return [6]byte{}, fmt.Errorf("expected 12 hex chars, got %d", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return [6]byte{}, err
	}
	var out [6]byte
	copy(out[:], b)
	return out, nil
}

// blockSizeToPrefixBits computes the number of significant MAC prefix bits from
// the block size (number of addresses in the range).
//
//	block_size = 2^(48 - prefixBits)  →  prefixBits = 48 - log2(block_size)
func blockSizeToPrefixBits(blockSize uint64) int {
	if blockSize == 0 {
		return 48
	}
	suffixBits := 0
	n := blockSize
	for n > 1 {
		n >>= 1
		suffixBits++
	}
	prefixBits := 48 - suffixBits
	if prefixBits < 0 {
		prefixBits = 0
	}
	if prefixBits > 48 {
		prefixBits = 48
	}
	return prefixBits
}

/* ---------- Helpers ---------- */

// writeTimestamp persists the current Unix time to the .last_update_mac marker file.
func writeTimestamp() {
	p := filepath.Join(dbDir, lastUpdateFile)
	if err := os.WriteFile(p, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0644); err != nil {
		log.Printf("Warning: could not write %s: %v", lastUpdateFile, err)
	}
}

// readAge returns how long ago the database was last built/downloaded.
// Returns max duration if the marker file is missing or unreadable.
func readAge() time.Duration {
	data, err := os.ReadFile(filepath.Join(dbDir, lastUpdateFile))
	if err != nil {
		return 1<<63 - 1
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 1<<63 - 1
	}
	return time.Since(time.Unix(ts, 0))
}

// readLastModified returns the stored Last-Modified header value.
func readLastModified() string {
	data, err := os.ReadFile(filepath.Join(dbDir, lastModifiedFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeLastModified persists a Last-Modified header value.
func writeLastModified(value string) {
	if value == "" {
		return
	}
	p := filepath.Join(dbDir, lastModifiedFile)
	if err := os.WriteFile(p, []byte(value), 0644); err != nil {
		log.Printf("Warning: could not write %s: %v", lastModifiedFile, err)
	}
}

// swapDB atomically replaces the in-memory reader and closes the old one.
func swapDB(newDB *maxminddb.Reader) {
	old := dbValue.Swap(newDB)
	if old != nil {
		if r, ok := old.(*maxminddb.Reader); ok {
			r.Close()
		}
	}
}

// installFile moves src to dst, falling back to a copy on cross-device rename.
func installFile(src, dst string) error {
	_ = os.Remove(dst)
	if err := os.Rename(src, dst); err != nil {
		return copyFile(src, dst)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

/* ---------- HTTP client with proxy support ---------- */

// newHTTPClient returns an *http.Client that honours HTTPS_PROXY / HTTP_PROXY / NO_PROXY.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// logProxyConfig logs the effective proxy URL for the given target URL, if any.
func logProxyConfig(targetURL string) {
	u, err := url.Parse(targetURL)
	if err != nil {
		return
	}
	req := &http.Request{URL: u}
	proxyURL, err := http.ProxyFromEnvironment(req)
	if err != nil || proxyURL == nil {
		log.Printf("Proxy: none (direct connection to %s)", u.Host)
		return
	}
	safe := *proxyURL
	if safe.User != nil {
		if _, hasPwd := safe.User.Password(); hasPwd {
			safe.User = url.UserPassword(safe.User.Username(), "***")
		}
	}
	log.Printf("Proxy: %s (for %s)", safe.String(), u.Host)
}

/* ---------- MAC Lookup ---------- */

// lookupMAC looks up a single MAC address string in the MMDB and returns a
// fully-populated MACAnswer. It never returns an error — any failure is encoded
// as valid=false or missing optional fields in the answer.
func lookupMAC(db *maxminddb.Reader, input string) *MACAnswer {
	ans := &MACAnswer{MAC: input}

	hw, valid := normalizeMAC(input)
	ans.Valid = valid

	if !valid {
		ans.MACType = "Unknown"
		return ans
	}

	// Normalise to canonical lowercase colon-separated form.
	ans.MAC = formatMAC(hw)
	ans.MACType = macAddressType(hw)
	ans.AdminType = macAdminType(hw)

	// Build the synthetic IPv6 lookup key and query the MMDB.
	ipv6 := macToIPv6(hw)
	var rec MACRecord
	if err := db.Lookup(ipv6, &rec); err != nil {
		log.Printf("MAC DB lookup error for %s: %v", ans.MAC, err)
		return ans
	}

	// If not found in DB, assignment will be empty string (zero value).
	if rec.Assignment == "" {
		ans.Registered = false
		return ans
	}

	bs := rec.BlockSize
	ans.Registered = registeredAssignments[rec.Assignment]
	ans.OUI = rec.OUI
	ans.OrgName = rec.OrgName
	ans.OrgAddress = rec.OrgAddress
	ans.CountryCode = strings.ToUpper(rec.CountryCode)
	ans.AddressMin = rec.AddressMin
	ans.AddressMax = rec.AddressMax
	ans.BlockSize = &bs
	ans.Assignment = rec.Assignment
	ans.Virtual = rec.Virtual

	return ans
}

/* ---------- HTTP Handlers ---------- */

func indexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func faviconHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Write(faviconPNG)
}

func openapiHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(openapiJSON)
}

func macHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Only POST allowed", http.StatusMethodNotAllowed)
			return
		}
		var req MACRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondMAC(w, "ERROR", nil)
			return
		}
		defer r.Body.Close()

		if len(req.MACs) > maxMACs {
			respondMAC(w, "ERROR", nil)
			return
		}

		dbVal := dbValue.Load()
		if dbVal == nil {
			respondMAC(w, "ERROR", nil)
			return
		}
		db := dbVal.(*maxminddb.Reader)

		var (
			answers   []MACAnswer
			foundCount int
		)

		switch {
		case req.MAC != nil && len(req.MACs) == 0:
			ans := lookupMAC(db, *req.MAC)
			answers = append(answers, *ans)
			if ans.Assignment != "" {
				foundCount++
			}
		case len(req.MACs) > 0 && req.MAC == nil:
			for _, macStr := range req.MACs {
				ans := lookupMAC(db, macStr)
				answers = append(answers, *ans)
				if ans.Assignment != "" {
					foundCount++
				}
			}
		default:
			respondMAC(w, "ERROR", nil)
			return
		}

		if len(answers) == 0 {
			respondMAC(w, "NOTFOUND", nil)
			return
		}
		if foundCount == 0 {
			respondMAC(w, "NOTFOUND", answers)
			return
		}
		respondMAC(w, "SUCCESS", answers)
	}
}

func respondMAC(w http.ResponseWriter, status string, answers []MACAnswer) {
	w.Header().Set("Content-Type", "application/json")
	resp := MACResponse{Status: status, Answers: answers}
	json.NewEncoder(w).Encode(resp)
}

// getDBHandler serves the current mac.mmdb file so peer instances can sync.
func getDBHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mmdbPath := filepath.Join(dbDir, dbFileName)
		if _, err := os.Stat(mmdbPath); err != nil {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		http.ServeFile(w, r, mmdbPath)
	}
}

/* ---------- DB — peer download mode ---------- */

// downloadFromPeer fetches mac.mmdb from the /db/mac endpoint of the configured
// peer instance (MAC_DB_URL), atomically swaps it into memory, and persists it.
func downloadFromPeer(ctx context.Context) error {
	u, err := url.Parse(dbURL)
	if err != nil {
		return fmt.Errorf("invalid MAC_DB_URL %q: %w", dbURL, err)
	}
	u.Path = "/db/mac"
	peerURL := u.String()
	log.Printf("Downloading mac.mmdb from peer: %s", peerURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, peerURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	client := newHTTPClient(120 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("peer GET %s: %w", peerURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer returned %s", resp.Status)
	}

	tmpFile, err := os.CreateTemp(dbDir, "mac-peer-*.mmdb")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmpFile.Name()
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write peer mmdb: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	newDB, err := maxminddb.Open(tmpName)
	if err != nil {
		return fmt.Errorf("open peer mmdb: %w", err)
	}

	swapDB(newDB)

	finalPath := filepath.Join(dbDir, dbFileName)
	if err := installFile(tmpName, finalPath); err != nil {
		log.Printf("Warning: could not persist peer mmdb: %v", err)
	}

	writeTimestamp()
	log.Println("Peer mmdb download complete")
	return nil
}

/* ---------- DB — CDN CSV build mode ---------- */

// errNotModified is returned by fetchCSVFromCDN when the CDN responds 304.
var errNotModified = errors.New("CSV not modified (304)")

// errRateLimited is returned by fetchCSVFromCDN when the CDN responds 429.
type errRateLimited struct {
	RetryAfter int64
}

func (e *errRateLimited) Error() string {
	return fmt.Sprintf("CDN rate-limited (429) — retry after unix timestamp %d (%s)",
		e.RetryAfter,
		time.Unix(e.RetryAfter, 0).UTC().Format(time.RFC3339))
}

// errProductGone is returned by fetchCSVFromCDN when the CDN responds 410 (Gone).
type errProductGone struct {
	Body string
}

func (e *errProductGone) Error() string {
	return fmt.Sprintf("CDN product gone (410): %s", e.Body)
}

// errUnauthorized is returned by fetchCSVFromCDN when the CDN responds 401.
type errUnauthorized struct {
	Message string
}

func (e *errUnauthorized) Error() string {
	return fmt.Sprintf("CDN unauthorized (401): %s", e.Message)
}

// extractJSONMessage attempts to extract the "message" field from a JSON body.
func extractJSONMessage(body []byte) string {
	var obj struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &obj); err == nil && obj.Message != "" {
		return obj.Message
	}
	return strings.TrimSpace(string(body))
}

// fetchCSVFromCDN fetches the gzipped MAC CSV from the CDN and returns a
// decompressed io.ReadCloser. Handles 304, 429, 410, and 401 per protocol.
func fetchCSVFromCDN(ctx context.Context) (io.ReadCloser, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cdnCSVURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create CDN request: %w", err)
	}

	req.Header.Set("User-Agent", "http2mac/1.0 (+https://github.com/letstool/http2mac)")

	if licenseKey != "" {
		req.Header.Set("Authorization", "Basic "+licenseKey)
	}

	if lm := readLastModified(); lm != "" {
		req.Header.Set("If-Modified-Since", lm)
		log.Printf("CDN request with If-Modified-Since: %s", lm)
	}

	client := newHTTPClient(180 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("CDN GET: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusNotModified: // 304
		resp.Body.Close()
		log.Println("CDN: CSV not modified (304) — current DB is up to date")
		return nil, "", errNotModified

	case http.StatusTooManyRequests: // 429
		ra := resp.Header.Get("Retry-After")
		resp.Body.Close()
		ts, _ := strconv.ParseInt(strings.TrimSpace(ra), 10, 64)
		return nil, "", &errRateLimited{RetryAfter: ts}

	case http.StatusGone: // 410
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, "", &errProductGone{Body: strings.TrimSpace(string(body))}

	case http.StatusUnauthorized: // 401
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, "", &errUnauthorized{Message: extractJSONMessage(body)}

	case http.StatusOK: // 200

	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		return nil, "", fmt.Errorf("CDN returned %s: %s", resp.Status, body)
	}

	lastModified := resp.Header.Get("Last-Modified")

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		resp.Body.Close()
		return nil, "", fmt.Errorf("CDN gzip reader: %w", err)
	}

	return &gzipReadCloser{gz: gz, body: resp.Body}, lastModified, nil
}

// gzipReadCloser wraps a gzip.Reader and its underlying HTTP response body.
type gzipReadCloser struct {
	gz   *gzip.Reader
	body io.ReadCloser
}

func (g *gzipReadCloser) Read(p []byte) (int, error) { return g.gz.Read(p) }
func (g *gzipReadCloser) Close() error {
	err1 := g.gz.Close()
	err2 := g.body.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// buildMACDBFromCSV fetches the gzipped MAC CSV from the CDN, parses it,
// compiles a fresh mac.mmdb, and atomically swaps it into memory.
//
// CSV columns (0-indexed):
//
//	0:oui  1:organisation_name  2:organization_address  3:country_code
//	4:address_min  5:address_max  6:block_size  7:assignment  8:virtual
func buildMACDBFromCSV(ctx context.Context) error {
	log.Printf("Fetching MAC CSV from CDN: %s", cdnCSVURL)

	csvReader, lastModified, err := fetchCSVFromCDN(ctx)
	if err != nil {
		if err == errNotModified {
			writeTimestamp()
			return nil
		}
		return fmt.Errorf("CDN fetch: %w", err)
	}
	defer csvReader.Close()

	writer, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:           "http2mac-MACDb",
		Description:            map[string]string{"en": "MAC Address OUI Database built by http2mac"},
		RecordSize:             28,
		IPVersion:              6,
		IncludeReservedNetworks: true,
	})
	if err != nil {
		return fmt.Errorf("create mmdb writer: %w", err)
	}

	r := csv.NewReader(csvReader)
	r.ReuseRecord = true
	r.FieldsPerRecord = 9 // oui, organisation_name, organization_address, country_code, address_min, address_max, block_size, assignment, virtual

	// Skip header row.
	if _, err := r.Read(); err != nil {
		return fmt.Errorf("read CSV header: %w", err)
	}

	inserted := 0
	lineNum := 1

	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("Warning: CSV parse error at line %d: %v — skipping", lineNum+1, err)
			lineNum++
			continue
		}
		lineNum++

		oui         := record[0]
		orgName     := record[1]
		orgAddr     := record[2]
		countryCode := record[3]
		addrMinStr  := record[4]
		addrMaxStr  := record[5]
		blockSizeStr := record[6]
		assignment  := record[7]
		virtual     := record[8]

		// Parse block_size to derive the prefix length.
		blockSize, err := strconv.ParseUint(strings.TrimSpace(blockSizeStr), 10, 64)
		if err != nil || blockSize == 0 {
			log.Printf("Warning: invalid block_size %q at line %d — skipping", blockSizeStr, lineNum)
			continue
		}

		// Parse address_min as the 6-byte network address.
		addrMinBytes, err := parseMACBytes(addrMinStr)
		if err != nil {
			log.Printf("Warning: invalid address_min %q at line %d — skipping", addrMinStr, lineNum)
			continue
		}

		// Compute MAC prefix bits from the block size.
		prefixBits := blockSizeToPrefixBits(blockSize)
		cidrBits := 80 + prefixBits

		// Build the synthetic IPv6 address: prefix(10) + addrMin(6).
		ip := make(net.IP, 16)
		copy(ip[0:10], ipv6Prefix[:])
		copy(ip[10:16], addrMinBytes[:])

		// Mask to the correct network address (ensures alignment).
		mask := net.CIDRMask(cidrBits, 128)
		network := &net.IPNet{
			IP:   ip.Mask(mask),
			Mask: mask,
		}

		macRecord := mmdbtype.Map{
			"oui":                  mmdbtype.String(oui),
			"organisation_name":    mmdbtype.String(orgName),
			"organization_address": mmdbtype.String(orgAddr),
			"country_code":         mmdbtype.String(countryCode),
			"address_min":          mmdbtype.String(addrMinStr),
			"address_max":          mmdbtype.String(addrMaxStr),
			"block_size":           mmdbtype.Uint64(blockSize),
			"assignment":           mmdbtype.String(assignment),
			"virtual":              mmdbtype.String(virtual),
		}

		if err := writer.Insert(network, macRecord); err != nil {
			log.Printf("Warning: failed to insert %s (line %d): %v", oui, lineNum, err)
			continue
		}
		inserted++
	}
	log.Printf("Parsed %d MAC prefix records from CDN CSV", inserted)

	// Write mmdb to a temp file, then atomically swap.
	tmpFile, err := os.CreateTemp(dbDir, "mac-build-*.mmdb")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmpFile.Name()
	defer os.Remove(tmpName)

	if _, err := writer.WriteTo(tmpFile); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write mmdb: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	newDB, err := maxminddb.Open(tmpName)
	if err != nil {
		return fmt.Errorf("open new mmdb: %w", err)
	}

	swapDB(newDB)

	finalPath := filepath.Join(dbDir, dbFileName)
	if err := installFile(tmpName, finalPath); err != nil {
		return fmt.Errorf("install mmdb: %w", err)
	}

	writeTimestamp()
	writeLastModified(lastModified)
	log.Printf("MAC DB built from CDN CSV: %d OUI prefix records inserted", inserted)
	return nil
}

/* ---------- DB — dispatch ---------- */

// updateDB calls the right update strategy depending on whether MAC_DB_URL is set.
func updateDB(ctx context.Context) error {
	if dbURL != "" {
		return downloadFromPeer(ctx)
	}
	return buildMACDBFromCSV(ctx)
}

// ensureDB loads the cached database if it is still within the refresh interval;
// otherwise calls updateDB to fetch or build a fresh copy.
func ensureDB(ctx context.Context) error {
	mmdbPath := filepath.Join(dbDir, dbFileName)
	if _, err := os.Stat(mmdbPath); err == nil {
		age := readAge()
		if age < updateInterval {
			db, err := maxminddb.Open(mmdbPath)
			if err != nil {
				return fmt.Errorf("open existing database: %w", err)
			}
			dbValue.Store(db)
			log.Printf("Loaded existing MAC DB (built %s ago, max age %s)",
				age.Round(time.Minute), updateInterval)
			return nil
		}
		log.Printf("MAC DB is %s old (max %s), updating...",
			age.Round(time.Minute), updateInterval)
	}
	return updateDB(ctx)
}

/* ---------- Scheduler ---------- */

var goneRetrySchedule = []time.Duration{
	24 * time.Hour,
	48 * time.Hour,
	72 * time.Hour,
	96 * time.Hour,
}

// schedulePeriodicUpdate fires updateDB every updateInterval (4 h).
// Handles 429, 410, and 401 CDN responses with appropriate back-off or permanent stop.
func schedulePeriodicUpdate(ctx context.Context) {
	mode := "CDN CSV build"
	if dbURL != "" {
		mode = "peer download (" + dbURL + ")"
	}
	log.Printf("MAC DB auto-refresh every %s [mode: %s]", updateInterval, mode)

	go func() {
		goneAttempt := 0

		timer := time.NewTimer(updateInterval)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				err := updateDB(ctx)

				if err == nil {
					if goneAttempt > 0 {
						log.Printf("CDN: update succeeded after %d gone-retry attempt(s) — counter reset", goneAttempt)
						goneAttempt = 0
					}
					timer.Reset(updateInterval)
					continue
				}

				// --- 429 rate-limited ---
				var rl *errRateLimited
				if errors.As(err, &rl) && rl.RetryAfter > 0 {
					wait := time.Until(time.Unix(rl.RetryAfter, 0))
					if wait <= 0 {
						wait = updateInterval
					}
					log.Printf("Rate-limited by CDN: next attempt in %s (at %s)",
						wait.Round(time.Second),
						time.Unix(rl.RetryAfter, 0).UTC().Format(time.RFC3339))
					timer.Reset(wait)
					continue
				}

				// --- 410 product gone/disabled ---
				var gone *errProductGone
				if errors.As(err, &gone) {
					if goneAttempt >= len(goneRetrySchedule) {
						log.Printf("CDN [410] Product is gone/disabled and all %d retry attempts have been exhausted — "+
							"stopping the update process permanently.", len(goneRetrySchedule))
						log.Printf("CDN [410] Last server message: %s", gone.Body)
						return
					}
					wait := goneRetrySchedule[goneAttempt]
					log.Printf("CDN [410] Product gone/disabled (attempt %d/%d) — next retry in %s.",
						goneAttempt+1, len(goneRetrySchedule), wait)
					if gone.Body != "" {
						log.Printf("CDN [410] Server message: %s", gone.Body)
					}
					goneAttempt++
					timer.Reset(wait)
					continue
				}

				// --- 401 unauthorized — stop permanently ---
				var unauth *errUnauthorized
				if errors.As(err, &unauth) {
					log.Printf("CDN [401] Authorization refused — stopping the update process permanently.")
					log.Printf("CDN [401] Server message: %s", unauth.Message)
					log.Printf("CDN [401] Please check your LICENSE_KEY / --license-key configuration.")
					return
				}

				// --- Any other error: log and retry at normal interval ---
				log.Printf("Scheduled update failed: %v — retrying in %s", err, updateInterval)
				timer.Reset(updateInterval)

			case <-ctx.Done():
				return
			}
		}
	}()
}

/* ---------- Main ---------- */

func main() {
	const sentinel = "\x00"
	flagDBURL      := flag.String("db-url",      sentinel, "Base URL of a peer http2mac instance (e.g. http://host:8080). Overrides MAC_DB_URL.")
	flagDBDir      := flag.String("db-dir",      sentinel, "Directory for the mmdb file. Overrides MAC_DB_DIR. Default: /data")
	flagListenAddr := flag.String("listen-addr", sentinel, "Listen address. Overrides LISTEN_ADDR. Default: 127.0.0.1:8080")
	flagLicenseKey := flag.String("license-key", sentinel, "CDN license key (Basic auth token). Overrides LICENSE_KEY. Optional.")
	flagMaxMACs    := flag.Int("max-macs",        -1,       "Max MACs per request. Overrides MAC_MAX_MACS. Default: 100")
	flag.Parse()

	resolve := func(flagVal, envKey, defaultVal string) string {
		if flagVal != sentinel {
			return flagVal
		}
		if v := os.Getenv(envKey); v != "" {
			return v
		}
		return defaultVal
	}

	dbURL      = resolve(*flagDBURL,      "MAC_DB_URL",  "")
	dbDir      = resolve(*flagDBDir,      "MAC_DB_DIR",  "/data")
	listenAddr = resolve(*flagListenAddr, "LISTEN_ADDR", "127.0.0.1:8080")
	licenseKey = resolve(*flagLicenseKey, "LICENSE_KEY", "")

	maxMACs = 100
	if *flagMaxMACs >= 0 {
		maxMACs = *flagMaxMACs
	} else if v := os.Getenv("MAC_MAX_MACS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxMACs = n
		}
	}

	switch {
	case dbURL != "":
		log.Printf("Mode: peer sync from %s (interval: %s)", dbURL, updateInterval)
		logProxyConfig(dbURL)
	case licenseKey != "":
		log.Printf("Mode: CDN CSV build — licensed (interval: %s)", updateInterval)
		logProxyConfig(cdnCSVURL)
	default:
		log.Printf("Mode: CDN CSV build — anonymous (interval: %s)", updateInterval)
		logProxyConfig(cdnCSVURL)
	}

	if err := os.MkdirAll(dbDir, 0755); err != nil {
		log.Fatalf("failed to create directory %s: %v", dbDir, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ensureDB(ctx); err != nil {
		log.Fatalf("failed to initialize MAC database: %v", err)
	}

	schedulePeriodicUpdate(ctx)

	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/favicon.png", faviconHandler)
	http.HandleFunc("/openapi.json", openapiHandler)
	http.HandleFunc("/api/v1/mac", macHandler())
	http.HandleFunc("/db/mac", getDBHandler())

	srv := &http.Server{
		Addr:         listenAddr,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	log.Printf("http2mac server listening on %s", listenAddr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}
