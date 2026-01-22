package main

import (
        "bufio"
        "encoding/base64"
        "encoding/json"
        "log"
        "net"
        "net/http"
        "strings"
        "sync"
        "time"

        "github.com/google/uuid"
        "github.com/gorilla/websocket"
)

/* ================= CONFIG ================= */

const (
        tcpAddr  = ":9000"
        webAddr  = ":9001"

        adminUser = "admin"
        adminPass = "admin" // 建議之後改成 env / hash
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
        ID      string `json:"id"`
        Command string `json:"command"`
        Target  string `json:"target"`
        State   string `json:"state"` // running / done
}

/* ================= GLOBAL ================= */

var (
        clients  = map[string]*Client{}
        clientMu sync.Mutex

        tasks  = map[string]*Task{}
        taskMu sync.Mutex

        wsClients   = map[*websocket.Conn]bool{}
        wsClientsMu sync.Mutex

        sessions   = map[string]bool{}
        sessionMu  sync.Mutex
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

func broadcast(event string, data any) {
        msg := map[string]any{
                "type": event,
                "data": data,
        }
        b, _ := json.Marshal(msg)

        wsClientsMu.Lock()
        for c := range wsClients {
                _ = c.WriteMessage(websocket.TextMessage, b)
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
        sessionMu.Lock()
        defer sessionMu.Unlock()
        return sessions[c.Value]
}

/* ================= TCP ================= */

func handleTCP(conn net.Conn) {
        defer conn.Close()

        reader := bufio.NewReader(conn)

        // hello
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

        // 單 device_id 會話
        clientMu.Lock()
        if old, ok := clients[c.DeviceID]; ok {
                old.Conn.Close()
        }
        clients[c.DeviceID] = c
        clientMu.Unlock()

        broadcast("client_online", c.DeviceID)

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
                        if cur, ok := clients[c.DeviceID]; ok && cur == c {
                                cur.LastSeen = time.Now()
                        }
                        clientMu.Unlock()

                case "result":
                        taskMu.Lock()
                        if t, ok := tasks[msg.TaskID]; ok {
                                t.State = "done"
                        }
                        taskMu.Unlock()

                        broadcast("task_result", msg)
                }
        }

        clientMu.Lock()
        if cur, ok := clients[c.DeviceID]; ok && cur == c {
                delete(clients, c.DeviceID)
                broadcast("client_offline", c.DeviceID)
        }
        clientMu.Unlock()
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

        wsClientsMu.Lock()
        wsClients[ws] = true
        wsClientsMu.Unlock()

        // init state
        clientMu.Lock()
        var cl []string
        for id := range clients {
                cl = append(cl, id)
        }
        clientMu.Unlock()

        taskMu.Lock()
        var tl []*Task
        for _, t := range tasks {
                tl = append(tl, t)
        }
        taskMu.Unlock()

        ws.WriteJSON(map[string]any{"type": "init_clients", "data": cl})
        ws.WriteJSON(map[string]any{"type": "init_tasks", "data": tl})

        for {
                if _, _, err := ws.ReadMessage(); err != nil {
                        break
                }
        }

        wsClientsMu.Lock()
        delete(wsClients, ws)
        wsClientsMu.Unlock()
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
        var req struct {
                User string `json:"user"`
                Pass string `json:"pass"`
        }
        _ = json.NewDecoder(r.Body).Decode(&req)

        if req.User != adminUser || req.Pass != adminPass {
                w.WriteHeader(http.StatusUnauthorized)
                return
        }

        sid := newSession()
        http.SetCookie(w, &http.Cookie{
                Name:     "SESSION",
                Value:    sid,
                Path:     "/",
                HttpOnly: true,
        })
}

func apiExec(w http.ResponseWriter, r *http.Request) {
        if !checkAuth(r) {
                w.WriteHeader(http.StatusUnauthorized)
                return
        }

        var req struct {
                Command string `json:"command"`
                Target  string `json:"target"`
        }
        _ = json.NewDecoder(r.Body).Decode(&req)

        taskID := uuid.New().String()
        task := &Task{
                ID:      taskID,
                Command: req.Command,
                Target:  req.Target,
                State:   "running",
        }

        taskMu.Lock()
        tasks[taskID] = task
        taskMu.Unlock()

        clientMu.Lock()
        for id, c := range clients {
                if req.Target != "" && req.Target != "ALL" && id != req.Target {
                        continue
                }
                msg := Message{
                        Type:    "task",
                        TaskID:  taskID,
                        Command: req.Command,
                }
                c.Writer.WriteString(encode(msg) + "\n")
                c.Writer.Flush()
        }
        clientMu.Unlock()

        broadcast("task_new", task)
}

/* ================= MAIN ================= */

func main() {
        // TCP
        go func() {
                ln, err := net.Listen("tcp", tcpAddr)
                if err != nil {
                        log.Fatal(err)
                }
                log.Println("TCP listen", tcpAddr)
                for {
                        conn, _ := ln.Accept()
                        go handleTCP(conn)
                }
        }()

        // Web
        http.HandleFunc("/api/login", loginHandler)
        http.HandleFunc("/api/exec", apiExec)
        http.HandleFunc("/ws", wsHandler)
        http.Handle("/", http.FileServer(http.Dir("./web")))

        log.Println("Web listen", webAddr)
        log.Fatal(http.ListenAndServe(webAddr, nil))
}
