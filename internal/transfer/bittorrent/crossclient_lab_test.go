package bittorrent

// Cross-client reference lab (BT-02).
//
// This file is the plumbing, not a test that runs in CI: every entry point skips
// unless PROVENANCE_TEST_BITTORRENT_CROSSCLIENT=1 is set, and skips again if the
// pinned qBittorrent image is not already present locally (the lab never pulls).
//
// The lab is deliberately built out of parts that are boring to reason about:
//
//   - A dedicated docker bridge with masquerading disabled, so the container has
//     no route off the host (`enable_ip_masquerade=false`), and its resolver is
//     pointed at 127.0.0.1 so it cannot resolve names either. Only the host may
//     reach it, over the bridge address.
//   - qBittorrent runs as the invoking user, from a bind-mounted config and data
//     directory, with DHT, PEX, LSD, uTP, queueing and ratio limits off and a
//     fixed listening port. No trackers are configured anywhere: peers are
//     introduced explicitly, so no announce leaves the host.
//   - The WebUI is the only control surface. Its first-run password is printed
//     to the container log; the lab reads it there, logs in, and keeps the
//     session cookie. Nothing is guessed and no credential is stored in the
//     repository.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// crossClientEnv opts into the lab. It is the only way these run.
	crossClientEnv = "PROVENANCE_TEST_BITTORRENT_CROSSCLIENT"

	// crossClientImageEnv overrides the client image. The default is the digest
	// the matrix in docs/CROSSCLIENT.md was measured with, not a floating tag:
	// `lscr.io/linuxserver/qbittorrent:latest` on 2026-11-01,
	// qBittorrent v5.3.0 / libtorrent 2.1.2.
	crossClientImageEnv = "PROVENANCE_TEST_BITTORRENT_CROSSCLIENT_IMAGE"

	defaultCrossClientImage = "lscr.io/linuxserver/qbittorrent@sha256:05c1454f9a32967affc1f34f67f8cc0cc57dd805dca45489fe797095a6286556"

	// labNetworkName is the isolated bridge the container joins. It is created
	// once and reused; masquerade stays off so no container packet is routed.
	labNetworkName = "provenance-bt-lab"

	// qbtListenPort is the container's BitTorrent listening port.
	qbtListenPort = 51413

	// qbtWebUIPort is the container's WebUI port.
	qbtWebUIPort = 8080
)

// qbtLab is one disposable qBittorrent instance plus the host-side directories
// it is allowed to touch.
type qbtLab struct {
	t       *testing.T
	image   string
	network string
	root    string // host directory mounted at /config and /data
	config  string // <root>/config -> /config
	data    string // <root>/data   -> /data
	name    string // container name

	ip     string
	base   string
	cookie string

	// actualPort is the peer port qBittorrent reports in its preferences.
	actualPort int

	// webPort and peerPort are the container's WebUI and BitTorrent ports. The
	// host-network mode needs its own pair: it shares the host's port space.
	webPort  int
	peerPort int
	// webPortNow is the port the container is currently asked to serve the
	// WebUI on: the first start uses qBittorrent's own default, and the lab's
	// value only takes effect once the config is patched.
	webPortNow int

	// hostNetwork runs the container on the host's network namespace. It is
	// needed only for the direction where Provenance itself is the seeder:
	// DiscoveryDirect binds the peer listener to loopback by design, so the
	// container has to share that loopback to reach it. Everything else runs on
	// the isolated bridge.
	hostNetwork bool

	started bool
}

// requireCrossClientLab skips before any work happens, so a disabled lab cannot
// create fixtures, dirs or goroutines that then fail for unrelated reasons.
func requireCrossClientLab(t *testing.T) {
	t.Helper()
	if os.Getenv(crossClientEnv) != "1" {
		t.Skipf("cross-client lab disabled; set %s=1 to run it", crossClientEnv)
	}
}

// newQbtLab skips the calling test unless the lab is opted into and everything
// it needs is already on the machine (no image pull, no network).
func newQbtLab(t *testing.T) *qbtLab {
	t.Helper()
	return newQbtLabOpts(t, labOptions{})
}

// labOptions selects how the container is attached to the network.
type labOptions struct {
	hostNetwork bool
}

func newQbtLabOpts(t *testing.T, opts labOptions) *qbtLab {
	t.Helper()
	if os.Getenv(crossClientEnv) != "1" {
		t.Skipf("cross-client lab disabled; set %s=1 to run it", crossClientEnv)
	}
	image := os.Getenv(crossClientImageEnv)
	if image == "" {
		image = defaultCrossClientImage
	}
	if !dockerAvailable(t) {
		t.Skip("cross-client lab needs a reachable docker daemon")
	}
	if !imagePresent(t, image) {
		t.Skipf("cross-client lab image %s is not present locally; the lab never pulls", image)
	}
	root := t.TempDir()
	l := &qbtLab{
		t:           t,
		image:       image,
		network:     labNetworkName,
		root:        root,
		config:      filepath.Join(root, "config"),
		data:        filepath.Join(root, "data"),
		name:        "provenance-bt-lab-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		hostNetwork: opts.hostNetwork,
		webPort:     qbtWebUIPort,
		peerPort:    qbtListenPort,
	}
	if opts.hostNetwork {
		// The host namespace already has services on the common ports.
		l.webPort = 18080
		l.peerPort = 61513
	}
	for _, dir := range []string{l.config, filepath.Join(l.data, "seed"), filepath.Join(l.data, "downloads")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("lab dir %s: %v", dir, err)
		}
	}
	t.Cleanup(l.stop)
	l.ensureNetwork()
	return l
}

func dockerAvailable(t *testing.T) bool {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	cmd := exec.Command("docker", "info", "--format", "{{.ServerVersion}}")
	cmd.Env = os.Environ()
	return cmd.Run() == nil
}

func imagePresent(t *testing.T, image string) bool {
	t.Helper()
	return exec.Command("docker", "image", "inspect", image).Run() == nil
}

// ensureNetwork creates the isolated bridge once. Masquerade is what makes the
// container unable to reach anything outside the host, so it must be off.
func (l *qbtLab) ensureNetwork() {
	l.t.Helper()
	if exec.Command("docker", "network", "inspect", l.network).Run() == nil {
		return
	}
	out, err := exec.Command("docker", "network", "create",
		"--label", "provenance.bt.lab=1",
		"--opt", "com.docker.network.bridge.enable_ip_masquerade=false",
		l.network).CombinedOutput()
	if err != nil {
		l.t.Fatalf("create lab network: %v\n%s", err, out)
	}
}

// prepare performs the two-phase configuration qBittorrent needs: it will only
// write its configuration file once it has started, so the lab starts it, stops
// it, merges the lab's settings into the file it wrote, and starts it again.
func (l *qbtLab) prepare() {
	l.t.Helper()
	l.run(qbtWebUIPort)
	l.waitConfig()
	l.stopContainer()
	l.patchConfig()
	l.run(l.webPort)
	l.login()
}

// waitConfig waits until qBittorrent has written its configuration file. The
// file appears early in startup, but not before the WebUI answers, so the lab
// polls for it rather than racing the first stop.
func (l *qbtLab) waitConfig() {
	l.t.Helper()
	path := filepath.Join(l.config, "qBittorrent", "config", "qBittorrent.conf")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	l.t.Fatalf("qBittorrent did not write %s", path)
}

// run starts the container and waits for the WebUI to answer.
func (l *qbtLab) run(webPort int) {
	l.t.Helper()
	networkArgs := []string{"--network", l.network, "--dns", "127.0.0.1"}
	if l.hostNetwork {
		// No published ports and no bridge: the container shares the host's
		// loopback, which is the only interface it is configured to use.
		networkArgs = []string{"--network", "host", "--dns", "127.0.0.1"}
	}
	args := append([]string{
		"run", "-d",
		"--name", l.name,
	}, append(networkArgs,
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-e", "HOME=/config",
		"-v", l.config+":/config",
		"-v", l.data+":/data",
		"--entrypoint", "/app/qbittorrent-nox",
		l.image,
		"--profile=/config", "--confirm-legal-notice",
		"--webui-port="+strconv.Itoa(webPort),
		"--torrenting-port="+strconv.Itoa(l.peerPort))...)
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		l.t.Fatalf("start container: %v\n%s", err, out)
	}
	l.started = true
	if l.hostNetwork {
		l.ip = "127.0.0.1"
	} else {
		l.ip = l.containerIP()
	}
	l.webPortNow = webPort
	l.base = "http://" + net.JoinHostPort(l.ip, strconv.Itoa(webPort))
	l.waitWebUI()
}

func (l *qbtLab) containerIP() string {
	l.t.Helper()
	out, err := exec.Command("docker", "inspect", "-f",
		"{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", l.name).Output()
	if err != nil {
		l.t.Fatalf("container ip: %v", err)
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		l.t.Fatal("container has no address on the lab network")
	}
	return ip
}

func (l *qbtLab) waitWebUI() {
	l.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(l.base + "/api/v2/app/version")
		if err == nil {
			_ = resp.Body.Close()
			// 403 is the expected answer before login: the WebUI is up.
			if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", l.name).CombinedOutput()
	l.t.Fatalf("qBittorrent WebUI did not answer at %s; container log:\n%s", l.base, logs)
}

// patchConfig merges the lab's settings into the configuration file qBittorrent
// wrote. Keys are inserted into their existing sections; qBittorrent rewrites
// this file itself, so the lab never assumes its exact layout.
func (l *qbtLab) patchConfig() {
	l.t.Helper()
	path := filepath.Join(l.config, "qBittorrent", "config", "qBittorrent.conf")
	data, err := os.ReadFile(path)
	if err != nil {
		l.t.Fatalf("read qBittorrent config: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")

	setKeys := func(section string, kv [][2]string) {
		header := "[" + section + "]"
		idx := -1
		for i, line := range lines {
			if strings.TrimSpace(line) == header {
				idx = i
				break
			}
		}
		if idx < 0 {
			if len(lines) > 0 && lines[len(lines)-1] != "" {
				lines = append(lines, "")
			}
			lines = append(lines, header)
			idx = len(lines) - 1
		}
		end := idx + 1
		for end < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[end]), "[") {
			end++
		}
		for _, pair := range kv {
			line := pair[0] + "=" + pair[1]
			replaced := false
			for i := idx + 1; i < end; i++ {
				if strings.HasPrefix(strings.TrimSpace(lines[i]), pair[0]+"=") {
					lines[i] = line
					replaced = true
					break
				}
			}
			if !replaced {
				lines = append(lines[:end], append([]string{line}, lines[end:]...)...)
				end++
			}
		}
	}

	prefs := [][2]string{
		{`WebUI\CSRFProtection`, "false"},
		{`WebUI\HostHeaderValidation`, "false"},
		{`WebUI\ClickjackingProtection`, "false"},
	}
	if l.hostNetwork {
		// The host namespace is shared, so nothing may be reachable from the
		// LAN: the WebUI and the peer listener bind loopback only.
		prefs = append(prefs,
			[2]string{`WebUI\Address`, "127.0.0.1"},
		)
	}
	setKeys("Preferences", prefs)
	bt := [][2]string{
		{`Session\DHTEnabled`, "false"},
		{`Session\PeXEnabled`, "false"},
		{`Session\LSDEnabled`, "false"},
		{`Session\uTPEnabled`, "false"},
		{`Session\BTProtocol`, "1"},
		{`Session\QueueingSystemEnabled`, "false"},
		{`Session\GlobalMaxRatio`, "-1"},
		{`Session\GlobalMaxSeedingMinutes`, "-1"},
		{`Session\AnonymousMode`, "false"},
		{`Session\Port`, strconv.Itoa(l.peerPort)},
		// 0 = prefer encryption, 1 = require, 2 = disable. The lab pins it so a
		// handshake failure is attributable instead of depending on a default.
		{`Session\Encryption`, "2"},
		// No port mapping, ever: the lab must not ask the router for anything.
		{`Session\UPnPEnabled`, "false"},
		{`Session\NATMPMPEnabled`, "false"},
		{`Session\DefaultSavePath`, "/data/downloads"},
		{`Session\TempPathEnabled`, "false"},
	}
	if l.hostNetwork {
		bt = append(bt,
			[2]string{`Session\Interface`, "lo"},
			[2]string{`Session\InterfaceName`, "lo"},
		)
	}
	setKeys("BitTorrent", bt)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		l.t.Fatalf("write qBittorrent config: %v", err)
	}
	l.t.Logf("lab: patched qBittorrent config at %s", path)
}

// login reads the first-run WebUI password from the container log and keeps the
// session cookie. The password is per-process and never written to the repo.
func (l *qbtLab) login() {
	l.t.Helper()
	out, err := exec.Command("docker", "logs", l.name).CombinedOutput()
	if err != nil {
		l.t.Fatalf("read container log: %v", err)
	}
	re := regexp.MustCompile(`temporary password is provided for this session:\s*(\S+)`)
	m := re.FindSubmatch(out)
	if m == nil {
		l.t.Fatalf("qBittorrent did not print a first-run password; log:\n%s", out)
	}
	password := string(m[1])
	form := url.Values{"username": {"admin"}, "password": {password}}
	req, err := http.NewRequest(http.MethodPost, l.base+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		l.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", l.base+"/")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		l.t.Fatalf("login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	// qBittorrent answers 204 on a successful login and 200 on some builds.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		l.t.Fatalf("login: status %d, body %q", resp.StatusCode, body)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "SID" || strings.HasPrefix(c.Name, "QBT_SID") {
			l.cookie = c.Name + "=" + c.Value
		}
	}
	if l.cookie == "" {
		l.t.Fatalf("login returned no session cookie; body %q", body)
	}
}

// appVersion reports the client's own version string, for the matrix.
func (l *qbtLab) appVersion(t *testing.T) string {
	t.Helper()
	out, err := l.api(http.MethodGet, "/api/v2/app/version", nil)
	if err != nil {
		t.Fatalf("app version: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// libtorrentVersion reports the peer-wire library's version, for the matrix.
func (l *qbtLab) libtorrentVersion(t *testing.T) string {
	t.Helper()
	out, err := l.api(http.MethodGet, "/api/v2/app/buildInfo", nil)
	if err != nil {
		t.Fatalf("build info: %v", err)
	}
	var info struct {
		Libtorrent string `json:"libtorrent"`
		Qt         string `json:"qt"`
		Boost      string `json:"boost"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "unknown"
	}
	return info.Libtorrent
}

// listenPort reports the port qBittorrent is actually accepting peers on, which
// is what the lab must dial (the configured value is a request, not a promise).
func (l *qbtLab) listenPort(t *testing.T) int {
	t.Helper()
	out, err := l.api(http.MethodGet, "/api/v2/app/preferences", nil)
	if err != nil {
		t.Fatalf("preferences: %v", err)
	}
	var prefs struct {
		ListenPort int  `json:"listen_port"`
		Encryption int  `json:"encryption"`
		UPnP       bool `json:"upnp"`
		NATPMP     bool `json:"natpmp"`
	}
	if err := json.Unmarshal(out, &prefs); err != nil {
		t.Fatalf("decode preferences: %v", err)
	}
	l.t.Logf("lab: listen_port=%d encryption=%d upnp=%t natpmp=%t",
		prefs.ListenPort, prefs.Encryption, prefs.UPnP, prefs.NATPMP)
	return prefs.ListenPort
}

// probePeerPort dials the peer endpoint from the host and reports the outcome,
// so a transfer failure can be attributed to reachability rather than guessed at.
func (l *qbtLab) probePeerPort(t *testing.T) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", l.peerAddr(), 5*time.Second)
	if err != nil {
		t.Logf("lab: peer endpoint %s is NOT reachable from the host: %v", l.peerAddr(), err)
		return
	}
	_ = conn.Close()
	t.Logf("lab: peer endpoint %s accepts TCP", l.peerAddr())
}

// dumpTorrent logs what the reference client thinks of a torrent and which
// peers it is connected to. Only called on a failure path, to attribute it.
func (l *qbtLab) dumpTorrent(t *testing.T, hash string) {
	t.Helper()
	if list, err := l.torrents(); err == nil {
		for _, tor := range list {
			if strings.EqualFold(tor.Hash, hash) {
				t.Logf("lab torrent: %+v", tor)
			}
		}
	} else {
		t.Logf("lab torrents: %v", err)
	}
	out, err := l.api(http.MethodGet, "/api/v2/torrents/peers?hash="+url.QueryEscape(hash), nil)
	if err != nil {
		t.Logf("lab peers: %v", err)
		return
	}
	t.Logf("lab peers: %s", out)
}

// dumpLog prints the reference client's own application log, which is where an
// inbound peer rejection is recorded when there is one.
func (l *qbtLab) dumpLog(t *testing.T) {
	t.Helper()
	out, err := l.api(http.MethodGet,
		"/api/v2/log/main?normal=true&info=true&warning=true&critical=true", nil)
	if err != nil {
		t.Logf("lab log: %v", err)
		return
	}
	var entries []struct {
		Message string `json:"message"`
		Type    int    `json:"type"`
	}
	if err := json.Unmarshal(out, &entries); err != nil {
		t.Logf("lab log (raw): %s", out)
		return
	}
	for _, e := range entries {
		t.Logf("lab log[%d]: %s", e.Type, e.Message)
	}
}

// api performs one authenticated WebUI call.
func (l *qbtLab) api(method, path string, form url.Values) ([]byte, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, l.base+path, body)
	if err != nil {
		return nil, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Referer", l.base+"/")
	req.Header.Set("Cookie", l.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return out, fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, out)
	}
	return out, nil
}

// addTorrent uploads a .torrent and waits until it is known, optionally paused.
func (l *qbtLab) addTorrent(torrentPath, savePath string, paused bool) {
	l.t.Helper()
	raw, err := os.ReadFile(torrentPath)
	if err != nil {
		l.t.Fatal(err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("torrents", filepath.Base(torrentPath))
	if err != nil {
		l.t.Fatal(err)
	}
	if _, err := fw.Write(raw); err != nil {
		l.t.Fatal(err)
	}
	_ = mw.WriteField("savepath", savePath)
	_ = mw.WriteField("paused", strconv.FormatBool(paused))
	_ = mw.WriteField("skip_checking", "false")
	_ = mw.WriteField("autoTMM", "false")
	if err := mw.Close(); err != nil {
		l.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, l.base+"/api/v2/torrents/add", &buf)
	if err != nil {
		l.t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Referer", l.base+"/")
	req.Header.Set("Cookie", l.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		l.t.Fatalf("add torrent: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		l.t.Fatalf("add torrent: status %d: %s", resp.StatusCode, out)
	}
}

// torrent is the subset of qBittorrent's /torrents/info record the lab reads.
type qbtTorrent struct {
	Hash         string  `json:"hash"`
	Name         string  `json:"name"`
	State        string  `json:"state"`
	Progress     float64 `json:"progress"`
	SavePath     string  `json:"save_path"`
	AmountLeft   int64   `json:"amount_left"`
	Uploaded     int64   `json:"uploaded"`
	Downloaded   int64   `json:"downloaded"`
	Peers        int     `json:"num_complete"` // seeds reported by the tracker/peer set
	Connections  int     `json:"num_leechs"`
	CompletionOn int64   `json:"completion_on"`
}

func (l *qbtLab) torrents() ([]qbtTorrent, error) {
	out, err := l.api(http.MethodGet, "/api/v2/torrents/info", nil)
	if err != nil {
		return nil, err
	}
	var list []qbtTorrent
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode torrents: %w (%s)", err, out)
	}
	return list, nil
}

// waitTorrent polls until the torrent satisfies cond, with a bounded wait.
func (l *qbtLab) waitTorrent(hash, what string, timeout time.Duration, cond func(qbtTorrent) bool) qbtTorrent {
	l.t.Helper()
	deadline := time.Now().Add(timeout)
	var last qbtTorrent
	var lastErr error
	for time.Now().Before(deadline) {
		list, err := l.torrents()
		if err != nil {
			lastErr = err
		} else {
			for _, tor := range list {
				if strings.EqualFold(tor.Hash, hash) {
					last = tor
					if cond(tor) {
						return tor
					}
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	l.t.Fatalf("qBittorrent did not reach %q for %s: last=%+v err=%v", what, hash, last, lastErr)
	return last
}

// waitComplete waits for a qBittorrent torrent to finish downloading.
func (l *qbtLab) waitComplete(hash string, timeout time.Duration) qbtTorrent {
	l.t.Helper()
	return l.waitTorrent(hash, "complete", timeout, func(tor qbtTorrent) bool {
		return tor.Progress >= 1 || tor.State == "uploading" || tor.State == "stalledUP" || tor.State == "pausedUP"
	})
}

// addMagnet adds a magnet URI, which is how the lab hands qBittorrent a peer
// hint (BEP 9 x.pe) without a tracker.
func (l *qbtLab) addMagnet(magnet, savePath string) {
	l.t.Helper()
	form := url.Values{"urls": {magnet}, "savepath": {savePath}, "paused": {"false"}, "autoTMM": {"false"}}
	if _, err := l.api(http.MethodPost, "/api/v2/torrents/add", form); err != nil {
		l.t.Fatalf("add magnet: %v", err)
	}
}

// fileIndices maps a torrent's file names to the indices the WebUI uses, so the
// lab never assumes an order.
func (l *qbtLab) fileIndices(hash string) (map[string]int, error) {
	files, err := l.files(hash)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(files))
	for i, f := range files {
		if name, ok := f["name"].(string); ok {
			out[name] = i
		}
	}
	return out, nil
}

// setFileNamePriorities sets priorities by file name: 1 download, 0 skip.
func (l *qbtLab) setFileNamePriorities(hash string, priorities map[string]int) {
	l.t.Helper()
	indices, err := l.fileIndices(hash)
	if err != nil {
		l.t.Fatalf("list files: %v", err)
	}
	byID := make(map[int]int, len(priorities))
	for name, prio := range priorities {
		id, ok := indices[name]
		if !ok {
			l.t.Fatalf("file %q is not in the torrent (have %v)", name, indices)
		}
		byID[id] = prio
	}
	l.setFilePriorities(hash, byID)
}

// setFilePriorities selects a subset in qBittorrent: 1 download, 0 skip. It is
// the reference client's selective-storage behaviour, not provenance's.
func (l *qbtLab) setFilePriorities(hash string, priorities map[int]int) {
	l.t.Helper()
	// One request per file: the batched form changed shape across WebUI
	// versions and answers "Priority must be an integer" when it does not match.
	for id, prio := range priorities {
		form := url.Values{
			"hash":     {hash},
			"id":       {strconv.Itoa(id)},
			"priority": {strconv.Itoa(prio)},
		}
		if _, err := l.api(http.MethodPost, "/api/v2/torrents/filePrio", form); err != nil {
			l.t.Fatalf("set priority of file %d: %v", id, err)
		}
	}
}

// jobSeedingReady reports whether the provenance job in staging has reached the
// seeding state, without failing when the record does not exist yet.
func jobSeedingReady(staging, infohash string) bool {
	path := filepath.Join(staging, filepath.FromSlash(torrentStateDir), "bt-"+infohash+".job.json")
	job, err := readJob(path)
	if err != nil {
		return false
	}
	return job.State.Seeding == SeedingActive
}

func (l *qbtLab) files(hash string) ([]map[string]any, error) {
	out, err := l.api(http.MethodGet, "/api/v2/torrents/files?hash="+url.QueryEscape(hash), nil)
	if err != nil {
		return nil, err
	}
	var files []map[string]any
	if err := json.Unmarshal(out, &files); err != nil {
		return nil, fmt.Errorf("decode files: %w (%s)", err, out)
	}
	return files, nil
}

// restart stops and starts the container against the same volumes, then logs in
// again (the WebUI password is per-process).
func (l *qbtLab) restart() {
	l.t.Helper()
	l.stopContainer()
	l.run(l.webPort)
	l.login()
}

func (l *qbtLab) stopContainer() {
	if !l.started {
		return
	}
	_ = exec.Command("docker", "rm", "-f", l.name).Run()
	l.started = false
}

// stop removes the container. The network is left alone: it is shared by
// concurrent labs and harmless to keep.
func (l *qbtLab) stop() { l.stopContainer() }

// peerAddr is the container's BitTorrent endpoint as the host must dial it. It
// is taken from the port qBittorrent reports, not from what the lab asked for.
func (l *qbtLab) peerAddr() string {
	port := l.actualPort
	if port == 0 {
		port = l.peerPort
	}
	return net.JoinHostPort(l.ip, strconv.Itoa(port))
}

// seedPayload copies a fixture tree into the container's seed directory so
// qBittorrent can verify and serve it without downloading anything.
func (l *qbtLab) seedPayload(name string, files map[string][]byte) {
	l.t.Helper()
	dir := filepath.Join(l.data, "seed", name)
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			l.t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			l.t.Fatal(err)
		}
	}
}

// downloadedPath is where a torrent's files land inside the mounted data dir.
func (l *qbtLab) downloadedPath(parts ...string) string {
	return filepath.Join(append([]string{l.data, "downloads"}, parts...)...)
}

// freePort asks the kernel for a port to give a host-side listener.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// hostBridgeIP is the address the container must dial to reach this host.
func (l *qbtLab) hostBridgeIP() string {
	l.t.Helper()
	out, err := exec.Command("docker", "network", "inspect", l.network,
		"-f", "{{(index .IPAM.Config 0).Gateway}}").Output()
	if err != nil {
		l.t.Fatalf("network gateway: %v", err)
	}
	gw := strings.TrimSpace(string(out))
	if gw == "" {
		l.t.Fatal("lab network reports no gateway")
	}
	return gw
}
