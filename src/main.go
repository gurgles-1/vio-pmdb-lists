package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimedefault"
	"github.com/hashicorp/go-hclog"
)

//go:embed manifest.json
var manifestJSON []byte

const configKey = "sync"
const taskKey = "sync-pmdb-lists"

type pluginConfig struct {
	PMDBAPIKey          string
	PMDBListIDs         []string
	TMDBAPIKey          string
	MovieLibraryID      int
	SeriesLibraryID     int
	SyncIntervalMinutes int
	StateFile           string
}

func (c pluginConfig) validate() error {
	if strings.TrimSpace(c.PMDBAPIKey) == "" {
		return fmt.Errorf("PublicMetaDB API key is not configured")
	}
	if len(c.PMDBListIDs) == 0 {
		return fmt.Errorf("no PublicMetaDB list IDs configured")
	}
	if strings.TrimSpace(c.TMDBAPIKey) == "" {
		return fmt.Errorf("TMDB API key is not configured")
	}
	if c.MovieLibraryID <= 0 && c.SeriesLibraryID <= 0 {
		return fmt.Errorf("no destination library configured; pick a Movies and/or Series library")
	}
	return nil
}

type runtimeServer struct {
	runtimedefault.Server
	pb.UnimplementedScheduledTaskServer
	pb.UnimplementedHttpRoutesServer
	manifest   *pb.PluginManifest
	mu         sync.Mutex
	cfg        pluginConfig
	lastSync   time.Time
	lastResult *syncResult
	logger     hclog.Logger
}

func (s *runtimeServer) GetManifest(context.Context, *pb.GetManifestRequest) (*pb.GetManifestResponse, error) {
	return &pb.GetManifestResponse{Manifest: s.manifest}, nil
}

// Configure receives the "sync" global config entry from Vio.
func (s *runtimeServer) Configure(_ context.Context, request *pb.ConfigureRequest) (*pb.ConfigureResponse, error) {
	cfg := pluginConfig{SyncIntervalMinutes: 720}
	for _, entry := range request.GetConfig() {
		if entry.GetKey() != configKey {
			continue
		}
		values := entry.GetValue().AsMap()
		cfg.PMDBAPIKey = strings.TrimSpace(strVal(values["pmdb_api_key"]))
		cfg.TMDBAPIKey = strings.TrimSpace(strVal(values["tmdb_api_key"]))
		cfg.PMDBListIDs = parseListIDs(values["pmdb_list_ids"])
		cfg.MovieLibraryID, _ = intVal(values["movie_library_id"])
		cfg.SeriesLibraryID, _ = intVal(values["series_library_id"])
		if n, ok := intVal(values["sync_interval_minutes"]); ok && n >= 15 {
			cfg.SyncIntervalMinutes = n
		}
		cfg.StateFile = strings.TrimSpace(strVal(values["state_file"]))
	}
	if err := cfg.validate(); err != nil {
		// Log but do not fail: the admin UI must stay usable so the
		// operator can fix the configuration.
		s.logger.Warn("PMDB Lists configuration incomplete", "error", err)
	}
	s.mu.Lock()
	s.cfg = cfg
	// Restore the last result for the admin page across restarts.
	if st := loadPersistedState(resolveStatePath(cfg.StateFile)); st.LastResult != nil {
		s.lastResult = st.LastResult
		s.lastSync = st.LastSync
	}
	s.mu.Unlock()
	return &pb.ConfigureResponse{}, nil
}

// Run handles the scheduled task trigger from Vio.
func (s *runtimeServer) Run(ctx context.Context, req *pb.RunScheduledTaskRequest) (*pb.RunScheduledTaskResponse, error) {
	if req.GetTaskKey() != "" && req.GetTaskKey() != taskKey {
		return nil, fmt.Errorf("unknown task key %q", req.GetTaskKey())
	}
	res, err := s.doSync(ctx, false)
	if err != nil {
		out, _ := structpb.NewStruct(map[string]any{"error": fmt.Sprintf("sync failed: %v", err)})
		return &pb.RunScheduledTaskResponse{Output: out}, err
	}
	out, _ := structpb.NewStruct(map[string]any{"summary": summarizeResult(res)})
	return &pb.RunScheduledTaskResponse{Output: out}, nil
}

func summarizeResult(res *syncResult) string {
	if res.Skipped {
		return "skipped: " + res.SkipReason
	}
	return fmt.Sprintf("registered=%d removed=%d lists=%d errors=%d reconciled=%v",
		res.Registered, res.Removed, len(res.Lists), len(res.Errors), res.Reconciled)
}

// --- admin HTTP routes ---

var secretParamPattern = regexp.MustCompile(`(?i)([?&](?:api_?key|apikey|token|secret|password|access_?token)=)[^&\s'"]+`)

func redactSecrets(s string) string {
	return secretParamPattern.ReplaceAllString(s, "${1}[redacted]")
}

func (s *runtimeServer) Handle(ctx context.Context, req *pb.HandleHTTPRequest) (*pb.HandleHTTPResponse, error) {
	role := req.GetHeaders()["X-Vio-User-Role"]
	if role == "" {
		role = req.GetHeaders()["X-Silo-User-Role"]
	}
	if !strings.EqualFold(role, "admin") {
		return httpJSON(http.StatusForbidden, map[string]string{"error": "admin access required"}), nil
	}
	path := strings.TrimRight(req.GetPath(), "/")
	switch {
	case path == "/admin/pmdb-lists" && req.GetMethod() == http.MethodGet:
		return httpHTML(adminPageHTML), nil
	case path == "/admin/pmdb-lists/status" && req.GetMethod() == http.MethodGet:
		return s.statusJSON(), nil
	case path == "/admin/pmdb-lists/sync" && req.GetMethod() == http.MethodPost:
		res, err := s.doSync(ctx, true)
		if err != nil {
			return httpJSON(http.StatusBadRequest, map[string]string{"error": err.Error()}), nil
		}
		return httpJSON(http.StatusOK, res), nil
	default:
		return httpJSON(http.StatusNotFound, map[string]string{"error": "not found"}), nil
	}
}

func (s *runtimeServer) statusJSON() *pb.HandleHTTPResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg
	out := map[string]any{
		"configured_lists":    len(cfg.PMDBListIDs),
		"list_ids":            cfg.PMDBListIDs,
		"movie_library_id":    cfg.MovieLibraryID,
		"series_library_id":   cfg.SeriesLibraryID,
		"sync_interval_minutes": cfg.SyncIntervalMinutes,
		"last_sync":           nil,
		"last_result":         nil,
	}
	if !s.lastSync.IsZero() {
		out["last_sync"] = s.lastSync.Format(time.RFC3339)
	}
	if s.lastResult != nil {
		out["last_result"] = s.lastResult
	}
	return httpJSON(http.StatusOK, out)
}

func httpJSON(status int, v any) *pb.HandleHTTPResponse {
	body, _ := json.MarshalIndent(v, "", "  ")
	return &pb.HandleHTTPResponse{
		StatusCode: int32(status),
		Headers:    map[string]string{"Content-Type": "application/json; charset=utf-8"},
		Body:       body,
	}
}

func httpHTML(body string) *pb.HandleHTTPResponse {
	return &pb.HandleHTTPResponse{
		StatusCode: 200,
		Headers:    map[string]string{"Content-Type": "text/html; charset=utf-8"},
		Body:       []byte(body),
	}
}

const adminPageHTML = `<!doctype html><html><head><meta charset="utf-8"><title>PMDB Lists</title>
<style>body{font-family:system-ui,sans-serif;max-width:720px;margin:2rem auto;padding:0 1rem;color:#e5e9f0;background:#0b0e14}
.card{background:#151a24;border:1px solid #2a3346;border-radius:8px;padding:1rem;margin-bottom:1rem}
button{background:#3b82f6;color:#fff;border:0;border-radius:6px;padding:.6rem 1.2rem;font-size:1rem;cursor:pointer}
button:disabled{opacity:.5}pre{background:#0b0e14;padding:1rem;border-radius:6px;overflow:auto;font-size:.85rem}</style>
</head><body><h1>PMDB Lists</h1>
<div class="card"><button id="sync">Sync now</button> <span id="msg"></span></div>
<div class="card"><h3>Status</h3><pre id="status">loading…</pre></div>
<script>
async function load(){const r=await fetch('/admin/pmdb-lists/status');document.getElementById('status').textContent=JSON.stringify(await r.json(),null,2)}
document.getElementById('sync').onclick=async(e)=>{e.target.disabled=true;document.getElementById('msg').textContent='syncing…';
const r=await fetch('/admin/pmdb-lists/sync',{method:'POST'});const j=await r.json();
document.getElementById('msg').textContent=r.error?('error: '+r.error):'done';e.target.disabled=false;load()};
load();</script></body></html>`

// --- config value helpers ---

func strVal(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func intVal(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		if t > 0 && t == float64(int(t)) {
			return int(t), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

func parseListIDs(v any) []string {
	var ids []string
	add := func(s string) {
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(strings.Trim(line, ","))
			if line != "" {
				ids = append(ids, line)
			}
		}
	}
	switch t := v.(type) {
	case string:
		add(t)
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				add(s)
			}
		}
	}
	return ids
}

// --- manifest + serve ---

func loadManifest() (*pb.PluginManifest, error) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		return nil, fmt.Errorf("load embedded manifest: %w", err)
	}
	if manifest.Checksum == "" {
		if executable, err := os.Executable(); err == nil {
			if binary, err := os.ReadFile(executable); err == nil {
				sum := sha256.Sum256(binary)
				manifest.Checksum = hex.EncodeToString(sum[:])
			}
		}
	}
	return manifest, nil
}

func main() {
	logger := hclog.New(&hclog.LoggerOptions{Name: "vio-pmdb-lists"})
	manifest, err := loadManifest()
	if err != nil {
		panic(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "manifest" {
		out, _ := json.MarshalIndent(manifest, "", "  ")
		fmt.Println(string(out))
		return
	}
	server := &runtimeServer{manifest: manifest, logger: logger}
	sdkruntime.Serve(sdkruntime.ServeConfig{
		Logger: logger,
		Servers: sdkruntime.CapabilityServers{
			Runtime:       server,
			ScheduledTask: server,
			HttpRoutes:    server,
		},
	})
}
