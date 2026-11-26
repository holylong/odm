// Package config stores ODM settings.
//
// The JSON value names deliberately mirror the registry value names observed in
// IDMan.exe (see re/FINDINGS.md §4), including the vendor's "Pasword" typo, so a
// settings file can be read/compared field-by-field against IDM's registry.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Proxy holds the per-scheme proxy triple IDM keeps (Http/Https/Ftp), plus SOCKS.
type Proxy struct {
	Enable      bool   `json:"ProxyEnable"`
	Server      string `json:"ProxyServer"`

	UseHttpProxy  bool   `json:"UseHttpProxy"`
	HttpProxy     string `json:"HttpProxy"`
	HttpPort      int    `json:"HttpPort"`
	HttpUserName  string `json:"HttpUserName"`
	HttpPasword   string `json:"HttpPasword"`

	UseHttpsProxy bool   `json:"UseHttpsProxy"`
	HttpsProxy    string `json:"HttpsProxy"`
	HttpsPort     int    `json:"HttpsPort"`
	HttpsUserName string `json:"HttpsUserName"`
	HttpsPasword  string `json:"HttpsPasword"`

	UseFtpProxy bool   `json:"UseFtpProxy"`
	FtpProxy    string `json:"FtpProxy"`
	FtpPort     int    `json:"FtpPort"`
	FtpUserName string `json:"FtpUserName"`
	FtpPasword  string `json:"FtpPasword"`
	FtpPasive   bool   `json:"FtpPasive"`

	UseSocks     bool   `json:"bUseSocks"`
	SocksType    int    `json:"SocksType"` // 4 or 5
	SocksHost    string `json:"SocksHost"`
	SocksPort    int    `json:"SocksPort"`
	SocksUser    string `json:"SocksUser"`
	SocksPass    string `json:"SocksPass"`
	SocksResolve bool   `json:"Socks5ProxyDNS"`
}

// Settings is the whole settings document.
type Settings struct {
	Version string `json:"Version"`

	// Connection tab
	MaxConnectionsNumber   int    `json:"MaxConnectionsNumber"`
	ConnectionType         string `json:"ConnectionType"`
	TimeoutSec             int    `json:"Timeout"`
	MaxConcurrentDownloads int    `json:"MaxConcurrentDownloads"`
	SpeedLimitKBps         int    `json:"SpeedLimitKBps"`
	MinSegmentSize         int64  `json:"MinSegmentSize"`
	MaxRetries             int    `json:"MaxRetries"`
	IgnoreModTimeChange    bool   `json:"IgnoreFileModificationTimeChanges"`

	// Identity
	UserAgent      string `json:"UserAgent"`
	AcceptLanguage string `json:"AcceptLanguage"`

	// Save To tab
	DefaultDir string `json:"DefaultDir"`
	TempDir    string `json:"TempDir"`

	// General tab
	AdvIntDriverEnabled2 bool `json:"AdvIntDriverEnabled2"`
	BridgePort           int  `json:"BridgePort"`
	ClipboardMonitor     bool `json:"ClipboardMonitor"`
	ShowStartDialog      bool `json:"ShowStartDialog"`
	ShowCompleteDialog   bool `json:"ShowCompleteDialog"`

	// Site logins (registered feature in IDM; here always on)
	Logins []Login `json:"Logins"`

	// File Types tab
	FileTypes FileTypes `json:"FileTypes"`

	// Proxy tab
	Proxy Proxy `json:"Proxy"`
}

// Login is a per-prefix credential.
type Login struct {
	URL      string `json:"URL"`
	UserName string `json:"UserName"`
	Password string `json:"Password"`
}

// FileTypes is the takeover policy observed in the File Types tab.
type FileTypes struct {
	// AutoStart lists extensions IDM takes over from a browser, e.g. "zip,rar,7z".
	AutoStart []string `json:"AutoStart"`
	// ExcludedSites uses `*` wildcards, e.g. "*.tonec.com".
	ExcludedSites   []string `json:"ExcludedSites"`
	ExcludedAddress []string `json:"ExcludedAddress"`
}

var (
	mu      sync.RWMutex
	current *Settings
)

// Default returns the shipped defaults. Values follow the observations in
// re/FINDINGS.md: 8 connections, identity encoding for segments, no takeover of
// html/php pages, `en-US` language.
func Default() *Settings {
	home, _ := os.UserHomeDir()
	dl := filepath.Join(home, "Downloads")
	return &Settings{
		Version:                "1.0",
		MaxConnectionsNumber:   8,
		ConnectionType:         "High speed: Direct connection (Ethernet/Cable) / Wi-Fi",
		TimeoutSec:             30,
		MaxConcurrentDownloads: 3,
		SpeedLimitKBps:         0,
		MinSegmentSize:         256 * 1024,
		MaxRetries:             5,
		IgnoreModTimeChange:    true,
		UserAgent:              "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
		AcceptLanguage:         "en-US,en;q=0.9",
		DefaultDir:             dl,
		TempDir:                filepath.Join(os.TempDir(), "odm"),
		BridgePort:             7749,
		ClipboardMonitor:       true,
		ShowStartDialog:        true,
		ShowCompleteDialog:     true,
		FileTypes: FileTypes{
			AutoStart: []string{
				"3gp", "7z", "aac", "ace", "apk", "arj", "avi", "bin", "bzip2", "deb", "dmg",
				"doc", "docx", "exe", "flv", "gif", "gz", "hqx", "iso", "jar", "jpg", "jpeg",
				"m4a", "m4v", "mkv", "mov", "mp3", "mp4", "mpeg", "mpg", "msi", "msp", "ogg",
				"pdf", "ppt", "pptx", "psd", "rar", "rm", "rmvb", "sit", "tar", "tgz", "torrent",
				"ttf", "wav", "webm", "wma", "wmv", "xls", "xlsx", "zip",
			},
			ExcludedSites:   []string{"*.update.microsoft.com", "*.windowsupdate.com"},
			ExcludedAddress: []string{},
		},
		Proxy: Proxy{FtpPasive: true, SocksType: 5, SocksResolve: true},
	}
}

// Path returns the settings file location.
func Path() string {
	if p := os.Getenv("ODM_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "ODM", "odm.json")
}

// Current returns the process-wide settings, loading them on first use.
func Current() *Settings {
	mu.RLock()
	if current != nil {
		s := current
		mu.RUnlock()
		return s
	}
	mu.RUnlock()
	mu.Lock()
	defer mu.Unlock()
	if current == nil {
		s := Default()
		if b, err := os.ReadFile(Path()); err == nil {
			_ = json.Unmarshal(b, s)
		}
		s.normalize()
		current = s
	}
	return current
}

// Set replaces the process-wide settings.
func Set(s *Settings) {
	mu.Lock()
	s.normalize()
	current = s
	mu.Unlock()
}

// Save persists the current settings.
func Save() error {
	s := Current()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	p := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

func (s *Settings) normalize() {
	if s.MaxConnectionsNumber < 1 {
		s.MaxConnectionsNumber = 1
	}
	if s.MaxConnectionsNumber > 32 {
		s.MaxConnectionsNumber = 32
	}
	if s.TimeoutSec < 5 {
		s.TimeoutSec = 5
	}
	if s.MaxConcurrentDownloads < 1 {
		s.MaxConcurrentDownloads = 1
	}
	if s.MinSegmentSize < 64*1024 {
		s.MinSegmentSize = 64 * 1024
	}
	if s.MaxRetries < 1 {
		s.MaxRetries = 1
	}
	if s.UserAgent == "" {
		s.UserAgent = Default().UserAgent
	}
	if s.AcceptLanguage == "" {
		s.AcceptLanguage = "en-US,en;q=0.9"
	}
	if s.BridgePort == 0 {
		s.BridgePort = 7749
	}
	if s.TempDir == "" {
		s.TempDir = filepath.Join(os.TempDir(), "odm")
	}
	if s.DefaultDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			s.DefaultDir = filepath.Join(home, "Downloads")
		}
	}
}
