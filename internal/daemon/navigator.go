package daemon

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/fsutil"
	"github.com/orlenko/aiq/internal/paths"
)

// The session navigator is served on a loopback port of its own, chosen at random at
// each start from those the navigator has not drawn lately, so a tab left open across a
// restart never reaches the new daemon at its old address. The port is not a secret.
// The daemon writes a fresh secret token for it to an owner-only file at each start.

// navHeader announces navigator support on /healthz, beside the identity header, which
// stays as it was. nav is the navigator's protocol version: 0 was a listener and token
// file with nothing behind them, and 1 adds /api/nav/hello behind its gate.
const (
	navHeader   = "X-Aiq-Nav"
	navProtocol = 1
)

const (
	navPortMin = 49152
	navPortMax = 65535
	// navPortsKept is how many drawn ports the navigator never draws again.
	navPortsKept = 256
	// navBindTries bounds the draws when the ports drawn are taken.
	navBindTries = 16
	// navPortsMaxBytes is far more than navPortsKept lines need.
	navPortsMaxBytes = 64 << 10
)

func navPortsPath() string { return filepath.Join(paths.DataDir(), "ui-ports") }

func navTokenPath() string { return filepath.Join(paths.DataDir(), "ui-token") }

// randomPort draws a port uniformly from the navigator's range. It is a variable so a
// test can choose the draws.
var randomPort = func() (int, error) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return navPortMin + int(binary.BigEndian.Uint16(b[:]))%(navPortMax-navPortMin+1), nil
}

// listenTCP binds a navigator port. It is a variable so a test can see what is on disk
// when the bind is tried, and what the bind gives back.
var listenTCP = func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }

// navPort is one line of ui-ports.
type navPort struct {
	port int
	at   int64
}

// readNavPorts reads the ports the navigator drew before. No file, or an empty one, is an
// empty list. A file that is not a regular one of the owner's alone, or that cannot be
// read, is an error, and so is one with content but no line that parses, blank lines
// included: the list keeps a tab from reaching a later daemon, so it is never taken as
// empty when it might not be. torn says some lines did not parse, which a crash in the
// middle of a write can leave.
func readNavPorts(path string) (ports []navPort, torn bool, err error) {
	f, err := openOwnerOnly(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, navPortsMaxBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > navPortsMaxBytes {
		return nil, false, fmt.Errorf("%s is larger than %d bytes", path, navPortsMaxBytes)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		p, ok := parseNavPort(line)
		if !ok {
			torn = true
			continue
		}
		ports = append(ports, p)
	}
	if len(b) > 0 && len(ports) == 0 {
		return nil, false, fmt.Errorf("%s has no line that reads as a port", path)
	}
	return ports, torn, nil
}

func parseNavPort(line string) (navPort, bool) {
	fields := strings.Split(line, " ")
	if len(fields) != 2 {
		return navPort{}, false
	}
	port, err := strconv.Atoi(fields[0])
	if err != nil || port <= 0 || port > 65535 || fields[0] != strconv.Itoa(port) {
		return navPort{}, false
	}
	at, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || fields[1] != strconv.FormatInt(at, 10) {
		return navPort{}, false
	}
	return navPort{port, at}, true
}

// writeNavPorts replaces ui-ports with the newest navPortsKept entries of ports.
func writeNavPorts(path string, ports []navPort) error {
	if len(ports) > navPortsKept {
		ports = ports[len(ports)-navPortsKept:]
	}
	var b strings.Builder
	for _, p := range ports {
		fmt.Fprintf(&b, "%d %d\n", p.port, p.at)
	}
	return fsutil.WriteFileAtomic(path, []byte(b.String()))
}

// bindNavigator binds the navigator's listener on a port drawn at random from those not
// in ui-ports, recording each port before trying it, so a crash between the two never
// leaves a used port out of the list. A port that cannot be bound costs a fresh draw,
// never the next port up, and never port 0, which the kernel could answer with a port
// just freed. Any bind error counts, not only a taken port: Windows refuses a port in a
// range it reserves with an access error.
func bindNavigator() (net.Listener, int, error) {
	path := navPortsPath()
	ports, _, err := readNavPorts(path)
	if err != nil {
		return nil, 0, fmt.Errorf("reading the navigator's port list: %w; remove or fix %s", err, path)
	}
	// Written below before any bind, which also drops the lines a torn write left.
	used := make(map[int]bool, len(ports)+navBindTries)
	for _, p := range ports {
		used[p.port] = true
	}
	var lastErr error
	for try := 0; try < navBindTries; try++ {
		port, err := drawNavPort(used)
		if err != nil {
			return nil, 0, err
		}
		used[port] = true
		ports = append(ports, navPort{port, time.Now().Unix()})
		if err := writeNavPorts(path, ports); err != nil {
			return nil, 0, fmt.Errorf("recording the navigator's port in %s: %w", path, err)
		}
		ln, err := listenTCP(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			lastErr = err
			continue
		}
		if a, ok := ln.Addr().(*net.TCPAddr); !ok || !a.IP.IsLoopback() || a.Port != port {
			ln.Close()
			return nil, 0, fmt.Errorf("the navigator bound to %v, which is not loopback port %d", ln.Addr(), port)
		}
		return ln, port, nil
	}
	return nil, 0, fmt.Errorf("no free port in %d draws, the last: %w", navBindTries, lastErr)
}

// drawNavPort draws until it finds a port not in used, which holds at most a few hundred
// of the 16,384.
func drawNavPort(used map[int]bool) (int, error) {
	for range 1000 {
		port, err := randomPort()
		if err != nil {
			return 0, err
		}
		if port < navPortMin || port > navPortMax {
			return 0, fmt.Errorf("drew port %d, outside %d-%d", port, navPortMin, navPortMax)
		}
		if !used[port] {
			return port, nil
		}
	}
	return 0, errors.New("drew only ports the navigator used lately")
}

// newNavToken is the navigator's secret for this start.
func newNavToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Navigator states in ui-token. Only a daemon holding the instance lock writes the file,
// so a start that loses the lock never changes it, and every start that holds the lock
// rewrites it, so a line naming another pid and start is stale.
const (
	navOn      = "on"      // served; the line ends with the port and the token
	navOff     = "off"     // turned off in the config
	navRefused = "refused" // daemon.listen was refused, so nothing is served
	navFailed  = "failed"  // not served: the navigator or the status page could not start or stopped
)

// writeNavState writes a state with no port or token.
func (s *Server) writeNavState(state string) error {
	return fsutil.WriteFileAtomic(navTokenPath(), []byte(fmt.Sprintf("%s %d %d\n", state, os.Getpid(), s.started.Unix())))
}

// failNavigator records that the navigator is not served. The file write is best effort,
// so the reason goes to the log either way.
func (s *Server) failNavigator(reason error) {
	s.log.Printf("navigator not served: %v", reason)
	if err := s.writeNavState(navFailed); err != nil {
		s.log.Printf("navigator: could not record that it is not served in %s: %v", navTokenPath(), err)
	}
}

// navigatorEnds records, as Run is about to return an error, that the navigator is not
// served. Only a navigator that was turned on says so in the log, so a daemon restarting
// over a taken status port does not point at a feature nobody turned on.
func (s *Server) navigatorEnds(reason error) {
	if s.cfg.UI.Navigator {
		s.failNavigator(reason)
		return
	}
	if err := s.writeNavState(navFailed); err != nil {
		s.log.Printf("navigator: could not record that it is not served in %s: %v", navTokenPath(), err)
	}
}

// startNavigator binds the navigator and writes its token file, or writes why it is not
// served. It returns nil when nothing is to be served.
func (s *Server) startNavigator() net.Listener {
	if !s.cfg.UI.Navigator {
		if err := s.writeNavState(navOff); err != nil {
			s.log.Printf("navigator: could not record that it is off in %s: %v", navTokenPath(), err)
		}
		return nil
	}
	ln, port, err := bindNavigator()
	if err != nil {
		s.failNavigator(err)
		return nil
	}
	token, err := newNavToken()
	if err == nil {
		err = fsutil.WriteFileAtomic(navTokenPath(), []byte(fmt.Sprintf("%s %d %d %d %s\n", navOn, os.Getpid(), s.started.Unix(), port, token)))
	}
	if err != nil {
		ln.Close()
		s.failNavigator(fmt.Errorf("writing %s: %w", navTokenPath(), err))
		return nil
	}
	s.mu.Lock()
	s.navPort, s.navToken = port, token
	s.mu.Unlock()
	s.log.Printf("aiq navigator listening on http://127.0.0.1:%d/", port)
	return ln
}

// serveNavigator serves the navigator until its listener fails. Then the navigator is
// gone, in the file and in /api/state at once, and the daemon goes on without it.
// Its server has the status page's deadlines and limits.
func (s *Server) serveNavigator(ln net.Listener) {
	err := serveNav(s.slots.server(s.navigatorHandler(), s.navigatorRefusal()), ln)
	ln.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.navPort, s.navToken = 0, ""
	s.failNavigator(fmt.Errorf("serving it: %w", err))
}
