package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"ffxban/internal/services/storage"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
)

var sshTermUpgrader = websocket.Upgrader{
	ReadBufferSize:  8 * 1024,
	WriteBufferSize: 64 * 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

type sshTermCtrl struct {
	Type    string `json:"type"`
	Cols    uint32 `json:"cols,omitempty"`
	Rows    uint32 `json:"rows,omitempty"`
	Message string `json:"message,omitempty"`
	Node    string `json:"node,omitempty"`
}

func sshTermSend(ws *websocket.Conn, typ, msg, node string) {
	data, _ := json.Marshal(sshTermCtrl{Type: typ, Message: msg, Node: node})
	ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = ws.WriteMessage(websocket.TextMessage, data)
}

func parseSSHSigner(privateKey string) (ssh.Signer, error) {
	privateKey = strings.TrimSpace(privateKey)
	if privateKey == "" {
		return nil, fmt.Errorf("empty private key")
	}
	if signer, err := ssh.ParsePrivateKey([]byte(privateKey)); err == nil {
		return signer, nil
	}
	decoded := strings.ReplaceAll(privateKey, `\n`, "\n")
	return ssh.ParsePrivateKey([]byte(decoded))
}

func buildSSHAuthMethods(privateKey, password string) ([]ssh.AuthMethod, error) {
	auth := make([]ssh.AuthMethod, 0, 3)

	if strings.TrimSpace(privateKey) != "" {
		signer, err := parseSSHSigner(privateKey)
		if err != nil {
			return nil, err
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}

	password = strings.TrimSpace(password)
	if password != "" {
		pw := password
		auth = append(auth,
			ssh.Password(pw),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = pw
				}
				return answers, nil
			}),
		)
	}

	return auth, nil
}

// resolveNodeSSHAddrAndName ищет ноду по key (ID или name), возвращает нормализованный адрес и имя.
func (s *Server) resolveNodeSSHAddrAndName(nodeKey string) (addr, name string) {
	if s.nodeSSH == nil {
		return "", ""
	}
	key := strings.ToLower(strings.TrimSpace(nodeKey))
	if s.limitProvider != nil {
		for _, node := range s.limitProvider.ListNodes() {
			id := strings.ToLower(strings.TrimSpace(node.ID))
			nm := strings.ToLower(strings.TrimSpace(node.Name))
			if id == key || nm == key {
				raw := strings.TrimSpace(node.Address)
				if raw == "" {
					return "", strings.TrimSpace(node.Name)
				}
				return s.nodeSSH.NormalizeAddress(raw), strings.TrimSpace(node.Name)
			}
		}
	}
	// Fallback: treat nodeKey as raw address
	if normalized := s.nodeSSH.NormalizeAddress(nodeKey); normalized != "" {
		return normalized, nodeKey
	}
	return "", ""
}

// resolveSSHCredsForNode возвращает user/password/private_key:
// per-node если есть, иначе глобальные.
func (s *Server) resolveSSHCredsForNode(ctx context.Context, nodeKey string) (user, password, privateKey string) {
	creds, found, _ := s.storage.GetNodeSSHCreds(ctx, nodeKey)
	if found && strings.TrimSpace(creds.User) != "" {
		pk := strings.TrimSpace(creds.PrivateKey)
		if pk == "" {
			pk = s.cfg.NodeSSHPrivateKey
		}
		pw := creds.Password
		if strings.TrimSpace(pw) == "" {
			pw = s.cfg.NodeSSHPassword
		}
		return strings.TrimSpace(creds.User), pw, pk
	}
	return strings.TrimSpace(s.cfg.NodeSSHUser), s.cfg.NodeSSHPassword, s.cfg.NodeSSHPrivateKey
}

// handleSSHTerminal обслуживает интерактивный SSH-терминал через WebSocket.
// GET /api/nodes/ssh/terminal?node_key=<key>[&cols=80&rows=24]
func (s *Server) handleSSHTerminal(c *gin.Context) {
	// Upgrade to WebSocket FIRST so errors can be sent as JSON control messages
	// (returning HTTP 4xx before upgrade causes ws.onerror in browser with no details)
	ws, err := sshTermUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("SSH terminal WS upgrade: %v", err)
		return
	}
	defer ws.Close()

	nodeKey := strings.TrimSpace(c.Query("node_key"))
	if nodeKey == "" {
		sshTermSend(ws, "error", "node_key is required", "")
		return
	}
	if s.nodeSSH == nil {
		sshTermSend(ws, "error", "SSH не настроен (NODE_SSH_ENABLED=false)", "")
		return
	}

	address, nodeName := s.resolveNodeSSHAddrAndName(nodeKey)
	if address == "" {
		sshTermSend(ws, "error", fmt.Sprintf("нода %q не найдена или не имеет адреса", nodeKey), nodeKey)
		return
	}

	user, password, privateKey := s.resolveSSHCredsForNode(c.Request.Context(), nodeKey)
	if user == "" {
		sshTermSend(ws, "error", "нет SSH-учётных данных: задайте user в ⚙ ноды или NODE_SSH_USER", nodeName)
		return
	}

	// Initial PTY size from query params
	cols, rows := parseTermSize(c.Query("cols"), c.Query("rows"))

	// Build SSH auth methods
	authMethods, err := buildSSHAuthMethods(privateKey, password)
	if err != nil {
		sshTermSend(ws, "error", fmt.Sprintf("SSH key parse: %v", err), nodeName)
		return
	}
	if len(authMethods) == 0 {
		sshTermSend(ws, "error", "no SSH auth method: set NODE_SSH_PRIVATE_KEY or password credentials", nodeName)
		return
	}

	sshConn, err := ssh.Dial("tcp", address, &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         12 * time.Second,
	})
	if err != nil {
		sshTermSend(ws, "error", fmt.Sprintf("SSH connect to %s: %v", address, err), nodeName)
		return
	}
	defer sshConn.Close()

	session, err := sshConn.NewSession()
	if err != nil {
		sshTermSend(ws, "error", fmt.Sprintf("SSH session: %v", err), nodeName)
		return
	}
	defer session.Close()

	if err := session.RequestPty("xterm-256color", int(rows), int(cols), ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 115200,
		ssh.TTY_OP_OSPEED: 115200,
	}); err != nil {
		sshTermSend(ws, "error", fmt.Sprintf("PTY request: %v", err), nodeName)
		return
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		sshTermSend(ws, "error", fmt.Sprintf("stdout pipe: %v", err), nodeName)
		return
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		sshTermSend(ws, "error", fmt.Sprintf("stdin pipe: %v", err), nodeName)
		return
	}

	if err := session.Shell(); err != nil {
		sshTermSend(ws, "error", fmt.Sprintf("shell start: %v", err), nodeName)
		return
	}

	// Notify client: connected
	connData, _ := json.Marshal(sshTermCtrl{Type: "connected", Node: nodeName, Cols: cols, Rows: rows})
	_ = ws.WriteMessage(websocket.TextMessage, connData)

	// Goroutine: SSH stdout → WS binary
	sshDone := make(chan struct{})
	go func() {
		defer close(sshDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Goroutine: WS → SSH stdin / resize
	wsDone := make(chan struct{})
	go func() {
		defer close(wsDone)
		ws.SetReadLimit(64 * 1024)
		for {
			msgType, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			switch msgType {
			case websocket.BinaryMessage:
				_, _ = stdin.Write(data)
			case websocket.TextMessage:
				var ctrl sshTermCtrl
				if json.Unmarshal(data, &ctrl) == nil {
					if ctrl.Type == "resize" && ctrl.Cols > 0 && ctrl.Rows > 0 {
						_ = session.WindowChange(int(ctrl.Rows), int(ctrl.Cols))
					} else if ctrl.Type == "ping" {
						pong, _ := json.Marshal(sshTermCtrl{Type: "pong"})
						ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
						_ = ws.WriteMessage(websocket.TextMessage, pong)
					}
				}
			}
		}
	}()

	select {
	case <-sshDone:
	case <-wsDone:
	}
	_ = stdin.Close()
	sshTermSend(ws, "closed", "", nodeName)
}

// handleSSHTerminalNodes возвращает список нод доступных для терминала.
func (s *Server) handleSSHTerminalNodes(c *gin.Context) {
	rows := make([]gin.H, 0)
	if s.limitProvider == nil || s.nodeSSH == nil {
		c.JSON(http.StatusOK, gin.H{"nodes": rows})
		return
	}
	allCreds, _ := s.storage.ListNodeSSHCreds(c.Request.Context())
	if allCreds == nil {
		allCreds = make(map[string]storage.NodeSSHCreds)
	}

	seen := make(map[string]struct{})
	for _, node := range s.limitProvider.ListNodes() {
		name := strings.TrimSpace(node.Name)
		id := strings.TrimSpace(node.ID)
		if name == "" && id == "" {
			continue
		}
		key := id
		if key == "" {
			key = name
		}
		if _, dup := seen[strings.ToLower(key)]; dup {
			continue
		}
		seen[strings.ToLower(key)] = struct{}{}

		address := strings.TrimSpace(node.Address)
		hasAddr := address != ""

		credKey := id
		if credKey == "" {
			credKey = name
		}
		_, hasCreds := allCreds[credKey]
		if !hasCreds {
			// Also check by name if ID lookup failed
			if id != "" {
				_, hasCreds = allCreds[name]
				if hasCreds {
					credKey = name
				}
			}
		}

		rows = append(rows, gin.H{
			"key":       key,
			"name":      name,
			"address":   address,
			"has_addr":  hasAddr,
			"has_creds": hasCreds,
		})
	}

	c.JSON(http.StatusOK, gin.H{"nodes": rows})
}

func parseTermSize(colsStr, rowsStr string) (cols, rows uint32) {
	cols, rows = 220, 50
	if c, ok := parseUint32(colsStr); ok && c >= 20 && c <= 500 {
		cols = c
	}
	if r, ok := parseUint32(rowsStr); ok && r >= 5 && r <= 200 {
		rows = r
	}
	return
}

func parseUint32(s string) (uint32, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	var v uint32
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + uint32(c-'0')
	}
	return v, true
}
