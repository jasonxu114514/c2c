package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

/* ================= CONFIG ================= */

const (
	tcpAddr = ":9000"
	webAddr = ":9001"

	adminUser = "admin"
	adminPass = "admin"

	heartbeatTimeout = 15 * time.Second
	heartbeatCheck   = 5 * time.Second
	wsSendBuf        = 32
)

/* ================= MODEL ================= */

type Message struct {
	Type     string `json:"type"`
	DeviceID string `json:"device_id,omitempty"`
	TaskID   string `json:"task_id,omitempty"`
	Command  string `json:"command,omitempty"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
}

type Client struct {
	DeviceID string
	Conn     net.Conn
	Writer   *bufio.Writer
	LastSeen time.Time
}

type Task struct {
	ID      string      `json:"id"`
	Command string      `json:"command"`
	Target  interface{} `json:"target"`
	State   string      `json:"state"`
}

/* ================= GLOBAL ================= */

var (
	clients  = map[string]*Client{}
	clientMu sync.RWMutex

	tasks  = map[string]*Task{}
	taskMu sync.RWMutex

	wsClients   = map[*websocket.Conn]chan []byte{}
	wsClientsMu sync.Mutex

	sessions  = map[string]bool{}
	sessionMu sync.RWMutex
)

/* ================= LOGGING ================= */

type JSONLogger struct {
	mu   sync.Mutex
	file *os.File
}

func NewJSONLogger(path string) *JSONLogger {
	_ = os.MkdirAll("log", 0755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		panic(err)
	}
	return &JSONLogger{file: f}
}

func (l *JSONLogger) Write(m map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m["ts"] = time.Now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(m)
	l.file.Write(b)
	l.file.Write([]byte("\n"))
}

var (
	historyLog = NewJSONLogger("log/history.log")
	taskLog    = NewJSONLogger("log/task.log")
	netLog     = NewJSONLogger("log/network.log")
)

/* ================= UTIL ================= */

func encode(v any) string {
	b, _ := json.Marshal(v)
	return base64.StdEncoding.EncodeToString(b)
}
func decode(s string, v any) error {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

/* ================= WS BROADCAST ================= */

func broadcast(event string, data any) {
	msg := map[string]any{"type": event, "data": data}
	b, _ := json.Marshal(msg)

	wsClientsMu.Lock()
	for _, ch := range wsClients {
		select {
		case ch <- b:
		default:
		}
	}
	wsClientsMu.Unlock()
}

/* ================= AUTH ================= */

func newSession() string {
	id := uuid.New().String()
	sessionMu.Lock()
	sessions[id] = true
	sessionMu.Unlock()
	return id
}
func checkAuth(r *http.Request) bool {
	c, err := r.Cookie("SESSION")
	if err != nil {
		return false
	}
	sessionMu.RLock()
	defer sessionMu.RUnlock()
	return sessions[c.Value]
}

/* ================= TCP ================= */

func handleTCP(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)

	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}

	var hello Message
	if decode(line, &hello) != nil || hello.Type != "hello" {
		return
	}

	c := &Client{
		DeviceID: hello.DeviceID,
		Conn:     conn,
		Writer:   bufio.NewWriter(conn),
		LastSeen: time.Now(),
	}

	clientMu.Lock()
	_, existed := clients[c.DeviceID]
	if old, ok := clients[c.DeviceID]; ok {
		old.Conn.Close()
	}
	clients[c.DeviceID] = c
	clientMu.Unlock()

	if !existed {
		netLog.Write(map[string]any{
			"event":     "client_online",
			"device_id": c.DeviceID,
		})
		broadcast("client_online", c.DeviceID)
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		var msg Message
		if decode(line, &msg) != nil {
			continue
		}

		switch msg.Type {
		case "ping":
			clientMu.Lock()
			if cur := clients[c.DeviceID]; cur == c {
				cur.LastSeen = time.Now()
			}
			clientMu.Unlock()

		case "result":
			taskMu.Lock()
			if t := tasks[msg.TaskID]; t != nil {
				t.State = "done"
			}
			taskMu.Unlock()

			clientMu.Lock()
			if cur := clients[c.DeviceID]; cur == c {
				cur.LastSeen = time.Now()
			}
			clientMu.Unlock()

			taskLog.Write(map[string]any{
				"task_id":   msg.TaskID,
				"device_id": c.DeviceID,
				"output":    msg.Output,
				"error":     msg.Error,
			})

			msg.DeviceID = c.DeviceID
			broadcast("task_result", msg)
		}
	}

	clientMu.Lock()
	if cur := clients[c.DeviceID]; cur == c {
		delete(clients, c.DeviceID)
		netLog.Write(map[string]any{
			"event":     "client_offline",
			"device_id": c.DeviceID,
		})
		broadcast("client_offline", c.DeviceID)
	}
	clientMu.Unlock()
}

/* ================= HEARTBEAT ================= */

func heartbeatWatcher() {
	t := time.NewTicker(heartbeatCheck)
	for range t.C {
		now := time.Now()
		clientMu.Lock()
		for id, c := range clients {
			if now.Sub(c.LastSeen) > heartbeatTimeout {
				c.Conn.Close()
				delete(clients, id)
				netLog.Write(map[string]any{
					"event":     "client_timeout",
					"device_id": id,
				})
				broadcast("client_offline", id)
			}
		}
		clientMu.Unlock()
	}
}

/* ================= WEB ================= */

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	if !checkAuth(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	send := make(chan []byte, wsSendBuf)

	wsClientsMu.Lock()
	wsClients[ws] = send
	wsClientsMu.Unlock()

	netLog.Write(map[string]any{
		"event":  "ws_connect",
		"remote": r.RemoteAddr,
	})

	go func() {
		for msg := range send {
			ws.WriteMessage(websocket.TextMessage, msg)
		}
	}()

	clientMu.RLock()
	var cl []string
	for id := range clients {
		cl = append(cl, id)
	}
	clientMu.RUnlock()

	ws.WriteJSON(map[string]any{"type": "init_clients", "data": cl})

	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			break
		}
	}

	wsClientsMu.Lock()
	delete(wsClients, ws)
	wsClientsMu.Unlock()
	close(send)
}

/* ================= API ================= */

func loginHandler(w http.ResponseWriter, r *http.Request) {
	var req struct{ User, Pass string }
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.User != adminUser || req.Pass != adminPass {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "SESSION", Value: newSession(), Path: "/", HttpOnly: true,
	})
}

func apiExec(w http.ResponseWriter, r *http.Request) {
	if !checkAuth(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var req struct {
		Command string      `json:"command"`
		Target  interface{} `json:"target"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	historyLog.Write(map[string]any{
		"user":    "admin",
		"command": req.Command,
		"target":  req.Target,
	})

	id := uuid.New().String()
	task := &Task{ID: id, Command: req.Command, Target: req.Target, State: "running"}
	taskMu.Lock()
	tasks[id] = task
	taskMu.Unlock()

	clientMu.RLock()
	for cid, c := range clients {
		if req.Target != "ALL" {
			if list, ok := req.Target.([]any); ok {
				ok2 := false
				for _, v := range list {
					if v == cid {
						ok2 = true
					}
				}
				if !ok2 {
					continue
				}
			}
		}
		c.Writer.WriteString(encode(Message{
			Type: "task", TaskID: id, Command: req.Command,
		}) + "\n")
		c.Writer.Flush()
	}
	clientMu.RUnlock()
}

func apiHistory(w http.ResponseWriter, r *http.Request) {
	if !checkAuth(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	f, err := os.Open("log/history.log")
	if err != nil {
		json.NewEncoder(w).Encode([]any{})
		return
	}
	defer f.Close()

	var res []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			res = append(res, m)
		}
	}
	if len(res) > 200 {
		res = res[len(res)-200:]
	}
	json.NewEncoder(w).Encode(res)
}

/* ================= MAIN ================= */

func main() {
	go heartbeatWatcher()

	go func() {
		ln, err := net.Listen("tcp", tcpAddr)
		if err != nil {
			log.Fatal(err)
		}
		for {
			conn, _ := ln.Accept()
			go handleTCP(conn)
		}
	}()

	http.HandleFunc("/api/login", loginHandler)
	http.HandleFunc("/api/exec", apiExec)
	http.HandleFunc("/api/history", apiHistory)
	http.HandleFunc("/ws", wsHandler)
	http.Handle("/", http.FileServer(http.Dir("./web")))

	log.Println("server listening")
	log.Fatal(http.ListenAndServe(webAddr, nil))
}
