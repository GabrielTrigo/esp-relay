package main

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed web/index.html
var webFS embed.FS

var buildVersion = "dev"

// basePath é o subpath padrão do painel (BASE_PATH). O X-Forwarded-Prefix do
// proxy tem prioridade quando presente.
var basePath string

// IOState é o IO como o servidor o conhece. `switch` carrega `state`;
// `number`/`text` carregam `value` + `value_updated_at` (ver MarshalJSON).
type IOState struct {
	Name string `json:"name"`
	// Kind vazio/desconhecido significa switch: estado antigo e IOs criados
	// pelo painel continuam se comportando como interruptor.
	Kind           string           `json:"kind"`
	State          bool             `json:"state"`
	UpdatedAt      time.Time        `json:"updated_at"`
	Value          *json.RawMessage `json:"value,omitempty"`
	ValueUpdatedAt *time.Time       `json:"value_updated_at,omitempty"`
}

// ioKind normaliza o kind: vazio ou desconhecido é switch.
func ioKind(kind string) string {
	switch kind {
	case "number", "text":
		return kind
	}
	return "switch"
}

// MarshalJSON emite `state` só para switch (inclusive false, que os clientes
// atuais esperam) e `value`/`value_updated_at` só para IO de valor.
func (io IOState) MarshalJSON() ([]byte, error) {
	if ioKind(io.Kind) == "switch" {
		return json.Marshal(struct {
			Name      string    `json:"name"`
			Kind      string    `json:"kind"`
			State     bool      `json:"state"`
			UpdatedAt time.Time `json:"updated_at"`
		}{io.Name, "switch", io.State, io.UpdatedAt})
	}
	// RawMessage copiado: o valor não pode ser reescrito no meio do marshal.
	var value json.RawMessage
	if io.Value != nil {
		value = append(json.RawMessage(nil), (*io.Value)...)
	}
	var valueAt *time.Time
	if io.ValueUpdatedAt != nil {
		v := io.ValueUpdatedAt.UTC()
		valueAt = &v
	}

	return json.Marshal(struct {
		Name           string          `json:"name"`
		Kind           string          `json:"kind"`
		Value          json.RawMessage `json:"value,omitempty"`
		ValueUpdatedAt *time.Time      `json:"value_updated_at,omitempty"`
	}{io.Name, ioKind(io.Kind), value, valueAt})
}

type PendingCommand struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	IO        string          `json:"io"`
	State     bool            `json:"state"`
	Value     json.RawMessage `json:"value,omitempty"` // só set_io de IO de valor
	CreatedAt time.Time       `json:"created_at"`
}

type DeviceState struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Online       bool                `json:"online"`
	LastSeen     time.Time           `json:"last_seen"`
	IOs          map[string]*IOState `json:"ios"`
	PendingQueue []PendingCommand    `json:"pending_queue"`
}

type StoreData struct {
	UpdatedAt time.Time               `json:"updated_at"`
	Devices   map[string]*DeviceState `json:"devices"`
}

type WSClient struct {
	deviceID string
	conn     *websocket.Conn
	mu       sync.Mutex
}

type SetIOMessage struct {
	Type      string `json:"type"`
	CommandID string `json:"command_id"`
	IO        string `json:"io"`
	// State fica sem omitempty: o ESP atual lê este campo para todo set_io e o
	// JSON não muda para switch (zero quebra). Em IO de valor ele é ignorado.
	State bool            `json:"state"`
	Value json.RawMessage `json:"value,omitempty"` // só number/text
}

func (c *WSClient) SendJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(v)
}

func (c *WSClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.Close()
}

type Hub struct {
	mu       sync.RWMutex
	filePath string
	apiKey   string
	data     StoreData
	clients  map[string]*WSClient
	upgrader websocket.Upgrader

	// writeSeq anda 1 antes e 1 depois de cada acesso ao arquivo de estado.
	// O wait group só acompanha as escritas que rodam FORA de h.mu (a maioria
	// roda dentro); é o que permite a um teste esperar o disco sossegar.
	writeSeq  int64
	inflight  sync.WaitGroup

	// subscribers são os painéis conectados via SSE (/events).
	subMU       sync.Mutex
	subscribers map[chan []byte]struct{}
}

func NewHub(filePath, apiKey string) (*Hub, error) {
	h := &Hub{
		filePath: filePath,
		apiKey:   apiKey,
		data: StoreData{
			UpdatedAt: time.Now(),
			Devices:   make(map[string]*DeviceState),
		},
		clients:     make(map[string]*WSClient),
		subscribers: make(map[chan []byte]struct{}),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}

	if err := h.loadOrInit(); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Hub) loadOrInit() error {
	if err := os.MkdirAll(filepath.Dir(h.filePath), 0o755); err != nil {
		return err
	}

	raw, err := os.ReadFile(h.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return h.saveAtomicLocked()
	}
	if err != nil {
		return err
	}

	if len(strings.TrimSpace(string(raw))) == 0 {
		return h.saveAtomicLocked()
	}

	var loaded StoreData
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return fmt.Errorf("falha ao parsear %s: %w", h.filePath, err)
	}
	if loaded.Devices == nil {
		loaded.Devices = make(map[string]*DeviceState)
	}

	for id, dev := range loaded.Devices {
		if dev.ID == "" {
			dev.ID = id
		}
		if dev.IOs == nil {
			dev.IOs = make(map[string]*IOState)
		}
		if dev.PendingQueue == nil {
			dev.PendingQueue = []PendingCommand{}
		}
		// Ao iniciar o servidor, nenhum socket está conectado ainda
		dev.Online = false
	}

	// Carregado fora de qualquer handler: normaliza em memória os timestamps
	// vindos do arquivo para UTC (§5).
	loaded.UpdatedAt = loaded.UpdatedAt.UTC()
	for _, dev := range loaded.Devices {
		dev.LastSeen = dev.LastSeen.UTC()
		for _, io := range dev.IOs {
			io.UpdatedAt = io.UpdatedAt.UTC()
		}
		for i := range dev.PendingQueue {
			dev.PendingQueue[i].CreatedAt = dev.PendingQueue[i].CreatedAt.UTC()
		}
	}

	h.data = loaded
	return h.saveAtomicLocked()
}

func (h *Hub) saveAtomicLocked() error {
	h.writeSeq++
	defer func() { h.writeSeq++ }()
	h.data.UpdatedAt = time.Now().UTC()
	payload, err := h.marshalStateLocked(false) // state.json: sem valores instantâneos
	if err != nil {
		return err
	}

	tmpPath := h.filePath + ".tmp"
	h.inflight.Add(1)
	defer h.inflight.Done()
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, h.filePath); err != nil {
		return err
	}
	h.broadcastLocked()
	return nil
}

// broadcastLocked notifica os painéis SSE com um estado completo. O evento usa
// JSON compacto: SSE exige que cada `data:` seja uma única linha.
func (h *Hub) broadcastLocked() {
	event, err := h.sseEventLocked(true)
	if err != nil {
		return
	}

	h.subMU.Lock()
	defer h.subMU.Unlock()
	for ch := range h.subscribers {
		select {
		case ch <- event:
		default: // painel lento: descarta em vez de travar o servidor
		}
	}
}

// sseEventLocked monta a linha `data:` com o estado atual (JSON compacto).
// O painel recebe os valores vivos (`withValues`), diferente do que é gravado.
func (h *Hub) sseEventLocked(withValues bool) ([]byte, error) {
	payload, err := h.marshalStateLocked(withValues)
	if err != nil {
		return nil, err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload); err != nil {
		return nil, err
	}
	return []byte("data: " + compact.String() + "\n\n"), nil
}

// marshalStateLocked serializa o estado em UTC sem mutar os dados compartilhados
// (§5, §110). Os timestamps são convertidos em uma cópia, de modo que a
// serialização pode ser chamada sob RLock (handlers GET) sem corrida de escrita.
// `withValues` distingue os dois usos: o painel/API quer os valores vivos de
// number/text; o state.json nunca os leva (só o kind).
func (h *Hub) marshalStateLocked(withValues bool) ([]byte, error) {
	clone := h.data
	clone.UpdatedAt = h.data.UpdatedAt.UTC()
	clone.Devices = make(map[string]*DeviceState, len(h.data.Devices))
	for id, dev := range h.data.Devices {
		copied := *dev
		copied.LastSeen = dev.LastSeen.UTC()
		copied.IOs = make(map[string]*IOState, len(dev.IOs))
		for ioKey, io := range dev.IOs {
			ioCopy := *io
			ioCopy.UpdatedAt = io.UpdatedAt.UTC()
			// Valor de number/text é instantâneo e não vai para o arquivo: um
			// report por segundo reescreveria o state.json sem parar. Só o kind
			// fica gravado; a memória viva (API/SSE) continua com o valor.
			if !withValues && ioKind(ioCopy.Kind) != "switch" {
				ioCopy.Value = nil
				ioCopy.ValueUpdatedAt = nil
			}
			copied.IOs[ioKey] = &ioCopy
		}
		copied.PendingQueue = make([]PendingCommand, len(dev.PendingQueue))
		copy(copied.PendingQueue, dev.PendingQueue)
		for i := range copied.PendingQueue {
			copied.PendingQueue[i].CreatedAt = dev.PendingQueue[i].CreatedAt.UTC()
		}
		clone.Devices[id] = &copied
	}
	return json.MarshalIndent(clone, "", "  ")
}

func (h *Hub) checkAPIKey(r *http.Request) bool {
	token := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if token == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			token = strings.TrimSpace(auth[7:])
		}
	}
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get("api_key"))
	}
	if token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(h.apiKey)) == 1
}

func (h *Hub) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.checkAPIKey(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

// handleEvents expõe o estado em Server-Sent Events para o painel (UI).
func (h *Hub) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !h.checkAPIKey(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan []byte, 16)
	h.subMU.Lock()
	h.subscribers[ch] = struct{}{}
	h.subMU.Unlock()
	defer func() {
		h.subMU.Lock()
		delete(h.subscribers, ch)
		h.subMU.Unlock()
	}()

	// Estado atual imediatamente, sem esperar a próxima mudança.
	h.mu.RLock()
	initial, err := h.sseEventLocked(true)
	h.mu.RUnlock()
	if err == nil {
		_, _ = w.Write(initial)
		flusher.Flush()
	}

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-ch:
			if _, err := w.Write(event); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (h *Hub) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// basePrefix devolve o caminho sob o qual o painel está montado, com barra
// final: "{{.Prefix}}". Vem do proxy reverso (X-Forwarded-Prefix) ou de
// BASE_PATH — o painel chama /api, /events e /ws por caminho absoluto, e sem
// isto um deploy em subpath (www.example.com/iot/) bateria no site vizinho.
func basePrefix(r *http.Request) string {
	prefix := strings.TrimSpace(r.Header.Get("X-Forwarded-Prefix"))
	if prefix == "" {
		prefix = basePath
	}
	// O header vem do proxy: só um caminho absoluto e simples é aceito, para
	// não injetar nada no <base href>.
	if prefix == "" || !strings.HasPrefix(prefix, "/") || strings.ContainsAny(prefix, "<>\"' ") {
		return "/"
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix
}

func (h *Hub) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	content, err := fs.ReadFile(webFS, "web/index.html")
	if err != nil {
		http.Error(w, "UI indisponível", http.StatusInternalServerError)
		return
	}
	// O HTML continua estático e sem html/template (colide com `${}`).
	page := strings.Replace(string(content), "<head>",
		"<head>\n  <base href=\""+html.EscapeString(basePrefix(r))+"\">", 1)
	page = strings.Replace(page, "{{BUILD_VERSION}}", html.EscapeString(buildVersion), 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(page))
}

// cloneIO devolve uma cópia do IO com os ponteiros de valor também copiados.
func cloneIO(io *IOState) *IOState {
	copied := *io
	if io.Value != nil {
		v := *io.Value
		copied.Value = &v
	}
	if io.ValueUpdatedAt != nil {
		t := *io.ValueUpdatedAt
		copied.ValueUpdatedAt = &t
	}
	return &copied
}

// cloneDevice devolve uma cópia profunda de um dispositivo, sem compartilhar
// ponteiros com o estado vivo (#1).
func cloneDevice(dev *DeviceState) *DeviceState {
	copied := *dev
	copied.IOs = make(map[string]*IOState, len(dev.IOs))
	for key, io := range dev.IOs {
		ioCopy := *io
		// O snapshot é encodado FORA do RLock: ponteiro compartilhado deixaria
		// `value` e `value_updated_at` de um report diferente no mesmo JSON.
		copied.IOs[key] = cloneIO(&ioCopy)
	}
	copied.PendingQueue = make([]PendingCommand, len(dev.PendingQueue))
	copy(copied.PendingQueue, dev.PendingQueue)
	return &copied
}

// snapshotDevicesLocked devolve cópias dos dispositivos para serialização fora do
// lock, sem nunca compartilhar ponteiros com o estado vivo (§24, #1).
func (h *Hub) snapshotDevicesLocked() []*DeviceState {
	list := make([]*DeviceState, 0, len(h.data.Devices))
	for _, dev := range h.data.Devices {
		list = append(list, cloneDevice(dev))
	}
	return list
}

func (h *Hub) handleListDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}

	h.mu.RLock()
	updatedAt := h.data.UpdatedAt
	list := h.snapshotDevicesLocked()
	h.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"updated_at": updatedAt,
		"devices":    list,
	})
}

func (h *Hub) handleDeviceByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/devices/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	deviceID := parts[0]

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
		case http.MethodPatch:
			h.handlePatchDevice(w, r, deviceID)
			return
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPatch)
			return
		}
		h.mu.RLock()
		dev, ok := h.data.Devices[deviceID]
		var snapshot *DeviceState
		if ok {
			snapshot = cloneDevice(dev)
		}
		h.mu.RUnlock()
		if !ok {
			http.Error(w, "device not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snapshot)
		return
	}

	if parts[1] == "io" {
		switch len(parts) {
		case 2:
			if r.Method != http.MethodPost {
				methodNotAllowed(w, http.MethodPost)
				return
			}
			h.handleSetIO(w, r, deviceID)
			return
		case 3:
			if r.Method != http.MethodDelete {
				methodNotAllowed(w, http.MethodDelete)
				return
			}
			ioKey, err := url.PathUnescape(parts[2])
			if err != nil || strings.TrimSpace(ioKey) == "" {
				http.Error(w, "io invalido", http.StatusBadRequest)
				return
			}
			h.handleDeleteIO(w, r, deviceID, ioKey)
			return
		default:
			http.NotFound(w, r)
			return
		}
	}

	http.NotFound(w, r)
}

// handleDeleteIO remove um IO do console/API. O firmware não é avisado (não há
// comando de remoção no protocolo): quem apaga a tabela é o firmware regravado.
// Enquanto ele anunciar o IO no register, o IO volta — por isso o painel avisa.
//
// O IO sai do estado e suas pendências morrem junto. Se era um switch ligado,
// anexa um `set_io` de volta ao "ligado": ESp com relé não pode ficar ligado
// por um botão que sumiu do painel. A lista de IOs entregue ao ESP na reconexão
// é o sync do register, então o firmware só converge na próxima conexão.
func (h *Hub) handleDeleteIO(w http.ResponseWriter, r *http.Request, deviceID, ioKey string) {
	now := time.Now().UTC()
	cmdID := fmt.Sprintf("cmd-%d", now.UnixNano())

	h.mu.Lock()
	dev, exists := h.data.Devices[deviceID]
	if !exists {
		h.mu.Unlock()
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	entry, ioExists := dev.IOs[ioKey]
	if !ioExists {
		h.mu.Unlock()
		http.Error(w, "io not found", http.StatusNotFound)
		return
	}
	wasOn := ioKind(entry.Kind) == "switch" && entry.State
	delete(dev.IOs, ioKey)

	// Pendências do IO removido morrem junto: não há mais como corrigi-las no painel.
	if len(dev.PendingQueue) > 0 {
		filtered := dev.PendingQueue[:0]
		for _, pending := range dev.PendingQueue {
			if pending.IO != ioKey {
				filtered = append(filtered, pending)
			}
		}
		dev.PendingQueue = filtered
	}
	cmd := PendingCommand{
		ID:        cmdID,
		Type:      "set_io",
		IO:        ioKey,
		CreatedAt: now,
	}
	client, connected := h.clients[deviceID]
	// O IO já saiu do painel; quando ele estava ligado o comando existe só para
	// não deixar o relé ligado no ESP depois que o botão sumiu.
	queued := wasOn && !(connected && dev.Online)
	if queued {
		dev.PendingQueue = append(dev.PendingQueue, cmd)
	}

	if err := h.saveAtomicLocked(); err != nil {
		h.mu.Unlock()
		http.Error(w, "failed to persist state", http.StatusInternalServerError)
		return
	}
	delivered := false
	if wasOn && !queued {
		message := SetIOMessage{Type: "set_io", CommandID: cmd.ID, IO: ioKey, State: false}
		h.mu.Unlock()
		if err := client.SendJSON(message); err != nil {
			log.Printf("[%s] falha ao enviar desligamento do io removido %q: %v", deviceID, ioKey, err)
			client.Close()
		} else {
			delivered = true
		}
	} else {
		h.mu.Unlock()
	}

	status := http.StatusOK
	if queued {
		status = http.StatusAccepted
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"device_id":  deviceID,
		"io":         ioKey,
		"delivered":  delivered,
		"queued":     queued,
		"command_id": cmdID,
	})
}

type PatchDeviceRequest struct {
	Name string `json:"name"`
}

// handlePatchDevice permite renomear um dispositivo existente (#10).
func (h *Hub) handlePatchDevice(w http.ResponseWriter, r *http.Request, deviceID string) {
	var req PatchDeviceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)

	h.mu.Lock()
	dev, ok := h.data.Devices[deviceID]
	if !ok {
		h.mu.Unlock()
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	if name != "" {
		dev.Name = name
	}
	if err := h.saveAtomicLocked(); err != nil {
		h.mu.Unlock()
		http.Error(w, "failed to persist state", http.StatusInternalServerError)
		return
	}
	snapshot := cloneDevice(dev)
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snapshot)
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

type SetIORequest struct {
	IO   string `json:"io"`
	Name string `json:"name,omitempty"`
	// Ponteiros distinguem "ausente" de "false": {"state":false} é um comando
	// válido para switch, e {"value":...} é o canal de IO de valor.
	State *bool           `json:"state,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

func (h *Hub) handleSetIO(w http.ResponseWriter, r *http.Request, deviceID string) {
	var req SetIORequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.IO) == "" {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	ioKey := strings.TrimSpace(req.IO)
	valueSet := len(req.Value) > 0
	stateSet := req.State != nil
	if valueSet == stateSet {
		// Nenhum dos dois, ou os dois: não há como saber o que o cliente quis.
		http.Error(w, "informe state ou value (exatamente um)", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	cmdID := fmt.Sprintf("cmd-%d", now.UnixNano())

	h.mu.Lock()
	dev, exists := h.data.Devices[deviceID]
	if !exists {
		h.mu.Unlock()
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}

	ioEntry, ioExists := dev.IOs[ioKey]
	// Um IO de valor não aceita comando de estado: `set_io` com `state` seria
	// ignorado pelo firmware e sujaria o estado do painel.
	if stateSet && ioExists && ioKind(ioEntry.Kind) != "switch" {
		h.mu.Unlock()
		http.Error(w, "io nao tem estado (use value)", http.StatusBadRequest)
		return
	}

	var value json.RawMessage
	if valueSet {
		// Sem kind conhecido não há como validar: o register deve vir antes.
		if !ioExists {
			h.mu.Unlock()
			http.Error(w, "io desconhecido para value", http.StatusBadRequest)
			return
		}
		kind := ioKind(ioEntry.Kind)
		if kind == "switch" {
			h.mu.Unlock()
			http.Error(w, "switch nao aceita value (use state)", http.StatusBadRequest)
			return
		}
		normalized, ok := validSetValue(kind, req.Value, maxSetValueTextBytes)
		if !ok {
			h.mu.Unlock()
			http.Error(w, "valor invalido para "+kind, http.StatusBadRequest)
			return
		}
		value = normalized
	}

	if !ioExists {
		ioName := req.Name
		if ioName == "" {
			ioName = ioKey
		}
		ioEntry = &IOState{
			Name: ioName,
		}
		dev.IOs[ioKey] = ioEntry
	} else if req.Name != "" {
		ioEntry.Name = req.Name
	}

	// Estado só existe em switch. IO de valor recebe o comando (fila/socket)
	// mas não guarda nada: um valor não é estado e não persiste.
	if stateSet {
		ioEntry.State = *req.State
		ioEntry.UpdatedAt = now
	}

	cmd := PendingCommand{
		ID:        cmdID,
		Type:      "set_io",
		IO:        ioKey,
		Value:     value,
		CreatedAt: now,
	}
	if stateSet {
		cmd.State = *req.State
	}

	client, connected := h.clients[deviceID]
	delivered := false

	// Garante que não reste comando contraditório para o mesmo IO depois do novo:
	// o ESP aplicaria o estado antigo por último (§7). Comandos iguais também são
	// removidos para não duplicar; o novo é sempre anexado no fim (FIFO).
	if len(dev.PendingQueue) > 0 {
		filtered := dev.PendingQueue[:0]
		for _, pending := range dev.PendingQueue {
			if !(pending.Type == "set_io" && pending.IO == ioKey) {
				filtered = append(filtered, pending)
			}
		}
		dev.PendingQueue = filtered
	}

	// Comando é enfileirado apenas quando não há como entregá-lo agora. Quando o
	// ESP está online o envio é imediato e um ack não é garantido, então manter o
	// item na fila faria o ESP reconectado receber pulsos obsoletos (§7).
	queued := !(connected && dev.Online)
	if queued {
		dev.PendingQueue = append(dev.PendingQueue, cmd)
	}

	if err := h.saveAtomicLocked(); err != nil {
		h.mu.Unlock()
		http.Error(w, "failed to persist state", http.StatusInternalServerError)
		return
	}

	if !queued {
		message := SetIOMessage{
			Type:      "set_io",
			CommandID: cmd.ID,
			IO:        ioKey,
			Value:     value,
		}
		if stateSet {
			message.State = *req.State
		}
		h.mu.Unlock()
		if err := client.SendJSON(message); err != nil {
			log.Printf("[%s] falha ao enviar comando via WS: %v", deviceID, err)
			client.Close()
		} else {
			delivered = true
		}
	} else {
		h.mu.Unlock()
	}

	status := http.StatusOK
	if !delivered {
		status = http.StatusAccepted
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp := map[string]any{
		"device_id": deviceID,
		"io":        ioKey,
		"delivered": delivered,
		// queued reflete a decisão real de enfileirar (device offline), que é
		// independente de o envio ter sido bem-sucedido.
		"queued":     queued,
		"command_id": cmd.ID,
	}
	if stateSet {
		resp["state"] = *req.State
	} else {
		resp["value"] = value
	}
	_ = json.NewEncoder(w).Encode(resp)
}

type WSIncoming struct {
	Type      string              `json:"type"`
	CommandID string              `json:"command_id,omitempty"`
	IO        string              `json:"io,omitempty"`
	State     bool                `json:"state,omitempty"`
	DeviceID  string              `json:"device_id,omitempty"`
	IOs       map[string]*IOState `json:"ios,omitempty"`
	Kind      string              `json:"kind,omitempty"`
	// Value fica cru para não perder 523 / 523.4 / "ok" num unmarshal genérico.
	Value json.RawMessage `json:"value,omitempty"`
}

func (h *Hub) handleWS(w http.ResponseWriter, r *http.Request) {
	if !h.checkAPIKey(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	deviceID := strings.TrimSpace(r.URL.Query().Get("device_id"))
	if deviceID == "" {
		http.Error(w, "missing device_id", http.StatusBadRequest)
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[%s] upgrade WS falhou: %v", deviceID, err)
		return
	}

	client := &WSClient{
		deviceID: deviceID,
		conn:     conn,
	}

	now := time.Now().UTC()

	h.mu.Lock()
	if prev, ok := h.clients[deviceID]; ok {
		prev.Close()
	}
	h.clients[deviceID] = client
	defer h.disconnectClient(deviceID, client)

	dev, exists := h.data.Devices[deviceID]
	if !exists {
		dev = &DeviceState{
			ID:           deviceID,
			Name:         deviceID,
			IOs:          make(map[string]*IOState),
			PendingQueue: []PendingCommand{},
		}
		h.data.Devices[deviceID] = dev
	}
	dev.Online = true
	dev.LastSeen = now

	syncPayload := h.syncPayloadLocked(deviceID, dev)
	queuedCopy := make([]PendingCommand, len(dev.PendingQueue))
	copy(queuedCopy, dev.PendingQueue)

	if err := h.saveAtomicLocked(); err != nil {
		log.Printf("[%s] erro ao salvar status online: %v", deviceID, err)
	}
	h.mu.Unlock()

	// 1. Envia estado sincronizado
	if err := client.SendJSON(syncPayload); err != nil {
		log.Printf("[%s] erro ao enviar sync: %v", deviceID, err)
		return
	}

	// 2. Drena a fila pendente na reconexão (remoção ocorre no ACK)
	for _, cmd := range queuedCopy {
		message := SetIOMessage{
			Type:      "set_io",
			CommandID: cmd.ID,
			IO:        cmd.IO,
			State:     cmd.State,
			Value:     cmd.Value,
		}
		if err := client.SendJSON(message); err != nil {
			log.Printf("[%s] erro ao drenar comando %s: %v", deviceID, cmd.ID, err)
			return
		}
	}

	for {
		// Derruba conexões zumbis: o firmware envia ping a cada 25s, então 90s
		// sem mensagem indica socket morto (§7, #7).
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, msgBytes, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var msg WSIncoming
		if err := json.Unmarshal(msgBytes, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "ping":
			// last_seen é atualizado apenas em memória: persisti-lo a cada ping
			// reescreveria o state.json inteiro em intervalos curtos (#2). O
			// valor é gravado no próximo ack/disconnect/save relevante.
			h.mu.Lock()
			if d, ok := h.data.Devices[deviceID]; ok {
				d.LastSeen = time.Now().UTC()
			}
			h.mu.Unlock()
			_ = client.SendJSON(map[string]string{"type": "pong"})

		case "ack":
			h.handleAck(deviceID, msg)

		case "register":
			h.handleRegister(deviceID, msg)

		case "report":
			h.handleReport(deviceID, msg)
		}
	}
}

// syncPayloadLocked monta o `sync` do firmware com os IOs que têm estado a
// reconciliar. IO `number`/`text` fica de fora: nada a aplicar, e mandar um
// `state:false` faria o ESP tratá-lo como interruptor. Chamar sob h.mu.
func (h *Hub) syncPayloadLocked(deviceID string, dev *DeviceState) map[string]any {
	ios := make(map[string]*IOState, len(dev.IOs))
	for key, io := range dev.IOs {
		if ioKind(io.Kind) == "switch" {
			ios[key] = io
		}
	}
	return map[string]any{
		"type":      "sync",
		"device_id": deviceID,
		"ios":       ios,
	}
}

// handleRegister registra os IOs anunciados pelo firmware. IOs já conhecidos
// preservam o estado desejado; IOs novos entram com o estado anunciado.
// Responde com o `sync` atualizado para o firmware aplicar.
func (h *Hub) handleRegister(deviceID string, msg WSIncoming) {
	h.mu.Lock()
	dev, ok := h.data.Devices[deviceID]
	if !ok {
		h.mu.Unlock()
		return
	}
	for key, announced := range msg.IOs {
		if key == "" || announced == nil {
			continue
		}
		kind := ioKind(announced.Kind)
		if existing, exists := dev.IOs[key]; exists {
			if announced.Name != "" {
				existing.Name = announced.Name
			}
			// Firmware regravado pode mudar o formato do IO. Sem isto, o kind
			// antigo ficaria para sempre e o painel nunca voltaria o toggle.
			if ioKind(existing.Kind) != kind {
				existing.Kind = kind
				if kind == "switch" {
					existing.Value = nil
					existing.ValueUpdatedAt = nil
				} else {
					existing.State = false
					existing.UpdatedAt = time.Time{}
				}
			}
			continue
		}
		name := announced.Name
		if name == "" {
			name = key
		}
		io := &IOState{Name: name, Kind: kind}
		if kind == "switch" {
			io.State = announced.State
			io.UpdatedAt = time.Now().UTC()
		}
		dev.IOs[key] = io
	}
	syncPayload := h.syncPayloadLocked(deviceID, dev)
	if err := h.saveAtomicLocked(); err != nil {
		log.Printf("[%s] erro ao persistir register: %v", deviceID, err)
	}
	h.mu.Unlock()

	client, ok := h.clients[deviceID]
	if !ok {
		return
	}
	if err := client.SendJSON(syncPayload); err != nil {
		log.Printf("[%s] erro ao enviar sync após register: %v", deviceID, err)
	}
}

// maxValueBytes espelha o teto da lib (kIotMaxTextBytes): um firmware pode não
// ser o nosso, então o servidor repete a checagem em vez de confiar.
const maxValueBytes = 4096

// maxSetValueTextBytes espelha kIotMaxReceivedTextBytes da lib (IotProtocol.h).
// Mudar um exige mudar o outro: o buffer do ESP é fixo, e um valor maior que
// ele seria truncado em silêncio. Testes nos dois lados travam o valor.
const maxSetValueTextBytes = 128

// validSetValue confere que o valor cru bate com o formato do IO e devolve a
// versão normalizada (a re-serialização compacta do que veio). `maxText` é o
// teto de string do canal (report aceita mais que set_io).
func validSetValue(kind string, raw json.RawMessage, maxText int) (json.RawMessage, bool) {
	// `null` decodifica sem erro em qualquer tipo destino (float64 vira 0,
	// string vira ""); a checagem do literal o barra antes do unmarshal.
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	if kind == "number" {
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, false
		}
		return raw, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || len(s) > maxText {
		return nil, false
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, false
	}
	return b, true
}

// validReportValue é o canal ESP -> gateway: aceita o teto maior do report.
func validReportValue(kind string, raw json.RawMessage) (json.RawMessage, bool) {
	return validSetValue(kind, raw, maxValueBytes)
}

// handleReport recebe um valor publicado pelo dispositivo. Não passa pela
// pending_queue (não é comando) e não grava em disco (um LDR a 1 Hz
// reescreveria o state.json sem parar): atualiza a memória e emite o SSE.
func (h *Hub) handleReport(deviceID string, msg WSIncoming) {
	// O firmware manda o kind junto: sem ele não há como saber se 734 é número
	// ou texto, e um firmware nosso sempre anuncia no register.
	if msg.Kind != "number" && msg.Kind != "text" {
		log.Printf("[%s] report descartado: kind %q invalido (io=%q)", deviceID, msg.Kind, msg.IO)
		return
	}
	kind := msg.Kind
	value, ok := validReportValue(kind, msg.Value)
	if !ok {
		log.Printf("[%s] report descartado: valor invalido para %s (io=%q)", deviceID, kind, msg.IO)
		return
	}
	if msg.IO == "" {
		log.Printf("[%s] report descartado: sem io", deviceID)
		return
	}

	now := time.Now().UTC()
	h.mu.Lock()
	dev, exists := h.data.Devices[deviceID]
	if !exists {
		h.mu.Unlock()
		log.Printf("[%s] report de dispositivo desconhecido", deviceID)
		return
	}
	dev.LastSeen = now
	entry, known := dev.IOs[msg.IO]
	if known && ioKind(entry.Kind) != kind {
		// O kind do register manda: um report number num switch do painel não
		// pode virar linha de valor, e vice-versa.
		atual := ioKind(entry.Kind)
		h.mu.Unlock()
		log.Printf("[%s] report descartado: io %q e %s, veio %s", deviceID, msg.IO, atual, kind)
		return
	}
	created := !known
	if created {
		// Defesa: o register deve vir antes. Criar aqui evita perder o valor,
		// mas nunca reatribui o kind de um IO já conhecido.
		entry = &IOState{Name: msg.IO, Kind: kind}
		dev.IOs[msg.IO] = entry
	}
	entry.Value = &value
	entry.ValueUpdatedAt = &now
	h.broadcastLocked()
	// Um IO criado aqui existe só nesta memória até o próximo restart; gravar o
	// kind agora (uma vez, na criação) é o mínimo para ele sobreviver. O valor
	// não vai junto: marshalStateLocked(false) o omite. Report repetido não salva.
	if created {
		if err := h.saveAtomicLocked(); err != nil {
			log.Printf("[%s] erro ao persistir IO criado por report: %v", deviceID, err)
		}
	}
	h.mu.Unlock()

	log.Printf("[%s] report io=%s value=%s", deviceID, msg.IO, string(value))
}

func (h *Hub) handleAck(deviceID string, msg WSIncoming) {
	now := time.Now().UTC()

	h.mu.Lock()
	defer h.mu.Unlock()

	dev, ok := h.data.Devices[deviceID]
	if !ok {
		return
	}
	dev.LastSeen = now

	// Localiza o comando confirmado para resolver io/state quando o ack vier
	// incompleto. `Value` na pendência marca uma confirmação de valor: esse ack
	// drena a fila, mas não grava estado (IO de valor não tem `state`).
	var confirmedIO string
	var confirmedState bool
	var confirmedValue json.RawMessage
	if msg.CommandID != "" {
		for _, item := range dev.PendingQueue {
			if item.ID == msg.CommandID {
				confirmedIO = item.IO
				confirmedState = item.State
				confirmedValue = item.Value
				break
			}
		}
	}

	if confirmedValue != nil {
		if msg.CommandID != "" && len(dev.PendingQueue) > 0 {
			filtered := dev.PendingQueue[:0]
			for _, item := range dev.PendingQueue {
				if item.ID != msg.CommandID {
					filtered = append(filtered, item)
				}
			}
			dev.PendingQueue = filtered
		}
		if err := h.saveAtomicLocked(); err != nil {
			log.Printf("[%s] erro ao persistir ACK de valor: %v", deviceID, err)
		}
		return
	}

	ioKey := msg.IO
	state := msg.State
	if ioKey == "" && confirmedIO != "" {
		ioKey = confirmedIO
		state = confirmedState
	}

	// IO de valor não tem `state`: um ack espontâneo (sem command_id) de um
	// firmware divergente não pode gravar estado nele. A drenagem de valor já
	// aconteceu acima, pelo `Value` da pendência.
	if ioKey != "" {
		if entry, exists := dev.IOs[ioKey]; exists && ioKind(entry.Kind) != "switch" {
			log.Printf("[%s] ack de estado ignorado em IO de valor %q", deviceID, ioKey)
			return
		}
	}

	// O IO foi removido do console (`DELETE .../io`): o ack só drena a
	// pendência (feito abaixo) e não pode ressuscitar o IO no state.json.
	if ioKey != "" {
		if _, exists := dev.IOs[ioKey]; !exists {
			log.Printf("[%s] ack de io removido %q ignorado", deviceID, ioKey)
			ioKey = ""
		}
	}

	if ioKey != "" {
		ioEntry := dev.IOs[ioKey]
		if ioEntry == nil {
			ioEntry = &IOState{Name: ioKey}
			dev.IOs[ioKey] = ioEntry
		}
		ioEntry.State = state
		ioEntry.UpdatedAt = now
	}

	if msg.CommandID != "" && len(dev.PendingQueue) > 0 {
		filtered := dev.PendingQueue[:0]
		for _, item := range dev.PendingQueue {
			if item.ID != msg.CommandID {
				filtered = append(filtered, item)
			}
		}
		dev.PendingQueue = filtered
	}

	if err := h.saveAtomicLocked(); err != nil {
		log.Printf("[%s] erro ao persistir ACK: %v", deviceID, err)
	}
}

func (h *Hub) disconnectClient(deviceID string, client *WSClient) {
	client.Close()

	h.mu.Lock()
	defer h.mu.Unlock()

	if current, ok := h.clients[deviceID]; ok && current == client {
		delete(h.clients, deviceID)
		if dev, exists := h.data.Devices[deviceID]; exists {
			dev.Online = false
			dev.LastSeen = time.Now().UTC()
			_ = h.saveAtomicLocked()
		}
	}
}

func main() {
	apiKey := strings.TrimSpace(os.Getenv("IOT_API_KEY"))
	if apiKey == "" {
		log.Fatal("A variável de ambiente IOT_API_KEY é obrigatória.")
	}

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	storePath := os.Getenv("STATE_FILE")
	if storePath == "" {
		storePath = filepath.Join("data", "state.json")
	}

	// Caminho sob o qual o painel é publicado (ex.: /iot). O proxy reverso manda
	// X-Forwarded-Prefix por requisição; BASE_PATH é o padrão para quem monta em
	// subpath sem proxy que injete o header. Na raiz, deixe vazio.
	basePath = strings.TrimSpace(os.Getenv("BASE_PATH"))

	hub, err := NewHub(storePath, apiKey)
	if err != nil {
		log.Fatalf("Erro ao iniciar Hub: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", hub.handleUI)
	mux.HandleFunc("/health", hub.handleHealth)
	mux.HandleFunc("/events", hub.handleEvents)
	mux.HandleFunc("/ws", hub.handleWS)
	mux.HandleFunc("/api/devices", hub.authMiddleware(hub.handleListDevices))
	mux.HandleFunc("/api/devices/", hub.authMiddleware(hub.handleDeviceByID))

	log.Printf("Servidor IoT WS rodando em %s (estado em %s)", addr, storePath)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("Erro no servidor HTTP: %v", err)
	}
}
