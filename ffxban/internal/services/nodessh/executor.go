package nodessh

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	defaultSSHPort    = 22
	defaultConnTO     = 8 * time.Second
	defaultOutputSize = 256 * 1024
)

// Config описывает параметры SSH-подключения к нодам.
type Config struct {
	Enabled        bool
	User           string
	Password       string
	PrivateKey     string
	DefaultPort    int
	ConnectTimeout time.Duration
	MaxOutputBytes int
}

// ExecResult содержит результат выполнения команды на ноде.
type ExecResult struct {
	Address  string
	Duration time.Duration
	ExitCode int
	Stdout   string
	Stderr   string
	Error    string
}

// StreamChunk описывает порцию live-вывода SSH-команды.
type StreamChunk struct {
	Stream string
	Line   string
}

// StreamCallback вызывается на каждую строку stdout/stderr во время выполнения.
type StreamCallback func(chunk StreamChunk)

// Executor выполняет команды на удаленных нодах по SSH.
type Executor struct {
	enabled        bool
	user           string
	defaultPort    int
	connectTimeout time.Duration
	maxOutputBytes int
	authMethods    []ssh.AuthMethod
}

func parsePrivateKeySigner(privateKey string) (ssh.Signer, error) {
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

// New создает SSH-исполнитель.
func New(cfg Config) (*Executor, error) {
	exec := &Executor{
		enabled:        cfg.Enabled,
		user:           strings.TrimSpace(cfg.User),
		defaultPort:    cfg.DefaultPort,
		connectTimeout: cfg.ConnectTimeout,
		maxOutputBytes: cfg.MaxOutputBytes,
	}
	if exec.defaultPort <= 0 {
		exec.defaultPort = defaultSSHPort
	}
	if exec.connectTimeout <= 0 {
		exec.connectTimeout = defaultConnTO
	}
	if exec.maxOutputBytes <= 0 {
		exec.maxOutputBytes = defaultOutputSize
	}

	if !exec.enabled {
		return exec, nil
	}
	if exec.user == "" {
		exec.enabled = false
		return exec, nil
	}

	privateKey := strings.TrimSpace(cfg.PrivateKey)
	if privateKey != "" {
		signer, err := parsePrivateKeySigner(privateKey)
		if err != nil {
			return nil, fmt.Errorf("parse ssh private key: %w", err)
		}
		exec.authMethods = append(exec.authMethods, ssh.PublicKeys(signer))
	}

	password := strings.TrimSpace(cfg.Password)
	if password != "" {
		exec.authMethods = append(exec.authMethods,
			ssh.Password(password),
			ssh.KeyboardInteractive(func(user, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					answers[i] = password
				}
				return answers, nil
			}),
		)
	}

	if len(exec.authMethods) == 0 {
		exec.enabled = false
	}
	return exec, nil
}

// Enabled показывает, активен ли SSH исполнитель.
func (e *Executor) Enabled() bool {
	return e != nil && e.enabled
}

// NormalizeAddress приводит адрес к формату host:port.
func (e *Executor) NormalizeAddress(address string) string {
	if e == nil {
		return normalizeAddress(address, defaultSSHPort)
	}
	return normalizeAddress(address, e.defaultPort)
}

// Execute выполняет команду на удаленной ноде.
func (e *Executor) Execute(ctx context.Context, address, command string, timeout time.Duration) ExecResult {
	return e.ExecuteStreaming(ctx, address, command, timeout, nil)
}

// ExecuteWithCredentials выполняет команду на ноде с явно указанными учётными данными.
// Используется для per-node SSH credentials, игнорирует глобальный user/password.
func (e *Executor) ExecuteWithCredentials(ctx context.Context, address, command, user, password string, timeout time.Duration) ExecResult {
	return e.ExecuteWithCredentialsStreaming(ctx, address, command, user, password, timeout, nil)
}

// ExecuteWithCredentialsAndPrivateKey выполняет команду на ноде с явными credentials:
// user + (password и/или private key).
func (e *Executor) ExecuteWithCredentialsAndPrivateKey(
	ctx context.Context,
	address, command, user, password, privateKey string,
	timeout time.Duration,
) ExecResult {
	return e.ExecuteWithCredentialsAndPrivateKeyStreaming(ctx, address, command, user, password, privateKey, timeout, nil)
}

// ExecuteStreaming выполняет команду и стримит stdout/stderr построчно через callback.
func (e *Executor) ExecuteStreaming(ctx context.Context, address, command string, timeout time.Duration, streamCb StreamCallback) ExecResult {
	res := ExecResult{
		Address:  strings.TrimSpace(address),
		ExitCode: -1,
	}
	started := time.Now()
	defer func() { res.Duration = time.Since(started) }()

	if !e.Enabled() {
		res.Error = "ssh disabled"
		return res
	}

	address = e.NormalizeAddress(address)
	if address == "" {
		res.Error = "empty node address"
		return res
	}
	command = strings.TrimSpace(command)
	if command == "" {
		res.Error = "empty command"
		return res
	}

	return e.executeWithAuth(ctx, address, command, e.user, e.authMethods, e.connectTimeout, e.maxOutputBytes, timeout, streamCb)
}

// ExecuteWithUserStreaming выполняет команду с явно указанным user,
// используя глобальные методы аутентификации (ключ/пароль из конфигурации).
func (e *Executor) ExecuteWithUserStreaming(
	ctx context.Context,
	address,
	command,
	user string,
	timeout time.Duration,
	streamCb StreamCallback,
) ExecResult {
	res := ExecResult{
		Address:  strings.TrimSpace(address),
		ExitCode: -1,
	}
	started := time.Now()
	defer func() { res.Duration = time.Since(started) }()

	if !e.Enabled() {
		res.Error = "ssh disabled"
		return res
	}

	address = e.NormalizeAddress(address)
	if address == "" {
		res.Error = "empty node address"
		return res
	}
	command = strings.TrimSpace(command)
	if command == "" {
		res.Error = "empty command"
		return res
	}
	user = strings.TrimSpace(user)
	if user == "" {
		user = e.user
	}
	if user == "" {
		res.Error = "empty ssh user"
		return res
	}

	return e.executeWithAuth(ctx, address, command, user, e.authMethods, e.connectTimeout, e.maxOutputBytes, timeout, streamCb)
}

// ExecuteWithCredentialsStreaming выполняет команду с явными credentials и live-стримом вывода.
func (e *Executor) ExecuteWithCredentialsStreaming(
	ctx context.Context,
	address,
	command,
	user,
	password string,
	timeout time.Duration,
	streamCb StreamCallback,
) ExecResult {
	return e.ExecuteWithCredentialsAndPrivateKeyStreaming(ctx, address, command, user, password, "", timeout, streamCb)
}

// ExecuteWithCredentialsAndPrivateKeyStreaming выполняет команду с явными credentials
// и live-стримом вывода. Поддерживает password и/или private key.
func (e *Executor) ExecuteWithCredentialsAndPrivateKeyStreaming(
	ctx context.Context,
	address,
	command,
	user,
	password,
	privateKey string,
	timeout time.Duration,
	streamCb StreamCallback,
) ExecResult {
	res := ExecResult{
		Address:  strings.TrimSpace(address),
		ExitCode: -1,
	}
	started := time.Now()
	defer func() { res.Duration = time.Since(started) }()

	if e == nil {
		res.Error = "ssh executor not initialized"
		return res
	}

	address = normalizeAddress(address, e.defaultPort)
	if address == "" {
		res.Error = "empty node address"
		return res
	}
	command = strings.TrimSpace(command)
	if command == "" {
		res.Error = "empty command"
		return res
	}

	user = strings.TrimSpace(user)
	if user == "" {
		res.Error = "empty ssh user"
		return res
	}

	authMethods, authErr := buildAuthMethods(privateKey, password)
	if authErr != nil {
		res.Error = authErr.Error()
		return res
	}
	if len(authMethods) == 0 {
		res.Error = "no ssh auth method provided"
		return res
	}

	connectTimeout := e.connectTimeout
	if connectTimeout <= 0 {
		connectTimeout = defaultConnTO
	}
	maxOutputBytes := e.maxOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultOutputSize
	}

	return e.executeWithAuth(ctx, address, command, user, authMethods, connectTimeout, maxOutputBytes, timeout, streamCb)
}

func (e *Executor) executeWithAuth(
	ctx context.Context,
	address,
	command,
	user string,
	authMethods []ssh.AuthMethod,
	connectTimeout time.Duration,
	maxOutputBytes int,
	timeout time.Duration,
	streamCb StreamCallback,
) ExecResult {
	res := ExecResult{
		Address:  strings.TrimSpace(address),
		ExitCode: -1,
	}
	started := time.Now()
	defer func() { res.Duration = time.Since(started) }()

	if strings.TrimSpace(address) == "" {
		res.Error = "empty node address"
		return res
	}
	if strings.TrimSpace(command) == "" {
		res.Error = "empty command"
		return res
	}
	if strings.TrimSpace(user) == "" {
		res.Error = "empty ssh user"
		return res
	}
	if len(authMethods) == 0 {
		res.Error = "no ssh auth method provided"
		return res
	}
	if connectTimeout <= 0 {
		connectTimeout = defaultConnTO
	}
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultOutputSize
	}

	clientConfig := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         connectTimeout,
	}

	conn, err := ssh.Dial("tcp", address, clientConfig)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer conn.Close()

	session, err := conn.NewSession()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer session.Close()

	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	stderrPipe, err := session.StderrPipe()
	if err != nil {
		res.Error = err.Error()
		return res
	}

	// Просим PTY: многие утилиты без TTY отключают ANSI-цвета.
	_ = session.RequestPty("xterm-256color", 140, 40, ssh.TerminalModes{
		ssh.ECHO:          0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	})

	stdout := newLimitedWriter(maxOutputBytes)
	stderr := newLimitedWriter(maxOutputBytes)

	var streamWg sync.WaitGroup
	streamWg.Add(2)
	go func() {
		defer streamWg.Done()
		readOutputStream(stdoutPipe, "stdout", stdout, streamCb)
	}()
	go func() {
		defer streamWg.Done()
		readOutputStream(stderrPipe, "stderr", stderr, streamCb)
	}()

	runCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	remoteCmd := "bash -lc " + strconv.Quote("export TERM=xterm-256color COLORTERM=truecolor CLICOLOR=1 CLICOLOR_FORCE=1 FORCE_COLOR=3; "+command)
	if err := session.Start(remoteCmd); err != nil {
		res.Error = err.Error()
		return res
	}

	done := make(chan error, 1)
	go func() {
		done <- session.Wait()
	}()

	var runErr error
	select {
	case runErr = <-done:
	case <-runCtx.Done():
		runErr = runCtx.Err()
		_ = session.Close()
		_ = conn.Close()
	}

	streamWg.Wait()
	res.Stdout = stdout.String()
	res.Stderr = stderr.String()

	if runErr == nil {
		res.ExitCode = 0
		return res
	}
	if errors.Is(runErr, context.DeadlineExceeded) {
		res.Error = "command timeout exceeded"
		return res
	}
	if errors.Is(runErr, context.Canceled) {
		res.Error = runErr.Error()
		return res
	}

	var exitErr *ssh.ExitError
	if errors.As(runErr, &exitErr) {
		res.ExitCode = exitErr.ExitStatus()
		res.Error = runErr.Error()
		return res
	}
	res.Error = runErr.Error()
	return res
}

func readOutputStream(r io.Reader, stream string, sink *limitedWriter, streamCb StreamCallback) {
	if r == nil || sink == nil {
		return
	}
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			_, _ = sink.Write([]byte(line))
			if streamCb != nil {
				streamCb(StreamChunk{
					Stream: stream,
					Line:   strings.TrimRight(line, "\r\n"),
				})
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			return
		}
	}
}

func authFromPassword(password string) []ssh.AuthMethod {
	password = strings.TrimSpace(password)
	if password == "" {
		return nil
	}
	return []ssh.AuthMethod{
		ssh.Password(password),
		ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i := range questions {
				answers[i] = password
			}
			return answers, nil
		}),
	}
}

func buildAuthMethods(privateKey, password string) ([]ssh.AuthMethod, error) {
	methods := make([]ssh.AuthMethod, 0, 3)
	privateKey = strings.TrimSpace(privateKey)
	if privateKey != "" {
		signer, err := parsePrivateKeySigner(privateKey)
		if err != nil {
			return nil, fmt.Errorf("parse ssh private key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	methods = append(methods, authFromPassword(password)...)
	return methods, nil
}

type limitedWriter struct {
	limit     int
	size      int
	truncated bool
	b         strings.Builder
}

func newLimitedWriter(limit int) *limitedWriter {
	if limit <= 0 {
		limit = defaultOutputSize
	}
	return &limitedWriter{limit: limit}
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	original := len(p)
	available := w.limit - w.size
	if available <= 0 {
		w.truncated = true
		return original, nil
	}
	if len(p) > available {
		p = p[:available]
		w.truncated = true
	}
	w.size += len(p)
	_, _ = w.b.Write(p)
	return original, nil
}

func (w *limitedWriter) String() string {
	if w == nil {
		return ""
	}
	s := w.b.String()
	if w.truncated {
		s += "\n... [output truncated]"
	}
	return s
}

func normalizeAddress(raw string, defaultPort int) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if defaultPort <= 0 {
		defaultPort = defaultSSHPort
	}

	if idx := strings.Index(value, "://"); idx > 0 {
		value = value[idx+3:]
	}
	if idx := strings.Index(value, "/"); idx >= 0 {
		value = value[:idx]
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	if _, _, err := net.SplitHostPort(value); err == nil {
		return value
	}

	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = strings.TrimPrefix(strings.TrimSuffix(value, "]"), "[")
	}
	if strings.Count(value, ":") > 1 && !strings.HasPrefix(value, "[") {
		return net.JoinHostPort(value, strconv.Itoa(defaultPort))
	}
	return net.JoinHostPort(value, strconv.Itoa(defaultPort))
}
