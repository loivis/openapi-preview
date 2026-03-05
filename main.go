package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const openAPIRelativeSuffix = "/docs/oas/openapi.yaml"

type spec struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Source   string `json:"source"`
	Size     int64  `json:"-"`
	Modified int64  `json:"-"`
}

type manifest struct {
	GeneratedAt string `json:"generatedAt"`
	Revision    string `json:"revision"`
	Items       []spec `json:"items"`
}

type catalog struct {
	root string

	mu       sync.RWMutex
	specs    []spec
	byID     map[string]spec
	revision string
}

func newCatalog(root string) *catalog {
	return &catalog{
		root: root,
		byID: make(map[string]spec),
	}
}

func (c *catalog) snapshot() manifest {
	c.mu.RLock()
	defer c.mu.RUnlock()

	items := make([]spec, len(c.specs))
	copy(items, c.specs)

	return manifest{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Revision:    c.revision,
		Items:       items,
	}
}

func (c *catalog) get(id string) (spec, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.byID[id]
	return s, ok
}

func (c *catalog) refresh() (bool, error) {
	discovered, err := discoverSpecs(c.root)
	if err != nil {
		return false, err
	}

	sort.Slice(discovered, func(i, j int) bool {
		return discovered[i].Name < discovered[j].Name
	})

	h := sha1.New()
	for _, s := range discovered {
		_, _ = h.Write([]byte(s.ID))
		_, _ = h.Write([]byte("|"))
		_, _ = h.Write([]byte(s.Source))
		_, _ = h.Write([]byte("|"))
		_, _ = h.Write(fmt.Appendf(nil, "%d|%d", s.Size, s.Modified))
		_, _ = h.Write([]byte("\n"))
	}
	revision := hex.EncodeToString(h.Sum(nil))

	c.mu.Lock()
	defer c.mu.Unlock()

	if revision == c.revision {
		return false, nil
	}

	byID := make(map[string]spec, len(discovered))
	for _, s := range discovered {
		byID[s.ID] = s
	}

	c.specs = discovered
	c.byID = byID
	c.revision = revision
	return true, nil
}

func discoverSpecs(root string) ([]spec, error) {
	var out []spec
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}

		if d.Name() != "openapi.yaml" {
			return nil
		}

		slashed := filepath.ToSlash(path)
		if !strings.HasSuffix(slashed, openAPIRelativeSuffix) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)

		service := strings.TrimSuffix(relSlash, openAPIRelativeSuffix)
		service = strings.Trim(service, "/")
		if service == "" {
			service = "."
		}

		idPrefix := sanitizeID(service)
		sum := sha1.Sum([]byte(relSlash))
		id := fmt.Sprintf("%s-%s", idPrefix, hex.EncodeToString(sum[:4]))

		out = append(out, spec{
			ID:       id,
			Name:     service,
			URL:      "/openapi/spec/" + url.PathEscape(id),
			Source:   relSlash,
			Size:     info.Size(),
			Modified: info.ModTime().UnixNano(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func sanitizeID(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := b.String()
	s = strings.Trim(s, "-")
	if s == "" {
		return "spec"
	}
	return s
}

type sseEvent struct {
	Name string
	Data string
}

type sseHub struct {
	mu   sync.Mutex
	subs map[chan sseEvent]struct{}
}

func newSSEHub() *sseHub {
	return &sseHub{
		subs: make(map[chan sseEvent]struct{}),
	}
}

func (h *sseHub) subscribe() chan sseEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan sseEvent, 8)
	h.subs[ch] = struct{}{}
	return ch
}

func (h *sseHub) unsubscribe(ch chan sseEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, ch)
	close(ch)
}

func (h *sseHub) broadcast(ev sseEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func main() {
	addr := getEnv("OAS_DOCS_ADDR", ":18080")

	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("getwd: %v", err)
	}

	root := getEnv("OAS_SEARCH_ROOT", filepath.Clean(filepath.Join(cwd, "..")))
	interval, err := time.ParseDuration(getEnv("OAS_SCAN_INTERVAL", "3s"))
	if err != nil {
		log.Fatalf("invalid OAS_SCAN_INTERVAL: %v", err)
	}

	catalog := newCatalog(root)
	changed, err := catalog.refresh()
	if err != nil {
		log.Fatalf("initial refresh: %v", err)
	}
	if changed {
		log.Printf("loaded specs: %d", len(catalog.snapshot().Items))
	}

	hub := newSSEHub()
	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go runWatcher(appCtx, catalog, hub, interval)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs", http.StatusFound)
	})
	mux.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(docsPage))
	})
	mux.HandleFunc("/openapi/manifest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		m := catalog.snapshot()
		_ = json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("/openapi/spec/", func(w http.ResponseWriter, r *http.Request) {
		idPart := strings.TrimPrefix(r.URL.Path, "/openapi/spec/")
		id, err := url.PathUnescape(idPart)
		if err != nil || id == "" {
			http.NotFound(w, r)
			return
		}

		s, ok := catalog.get(id)
		if !ok {
			http.NotFound(w, r)
			return
		}

		path := filepath.Join(root, filepath.FromSlash(s.Source))
		f, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "failed to open spec", http.StatusInternalServerError)
			return
		}
		defer f.Close()

		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		if _, err := io.Copy(w, f); err != nil {
			log.Printf("failed to stream spec %s: %v", s.ID, err)
		}
	})
	mux.HandleFunc("/openapi/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ch := hub.subscribe()
		defer hub.unsubscribe(ch)

		initData, _ := json.Marshal(map[string]string{
			"revision": catalog.snapshot().Revision,
		})
		fmt.Fprintf(w, "event: ready\ndata: %s\n\n", initData)
		flusher.Flush()

		keepAlive := time.NewTicker(25 * time.Second)
		defer keepAlive.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-keepAlive.C:
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			case ev := <-ch:
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, ev.Data)
				flusher.Flush()
			}
		}
	})

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	log.Printf("openapi docs server listening on %s", addr)
	log.Printf("search root: %s", root)

	go func() {
		<-appCtx.Done()
		log.Printf("shutdown requested, stopping server")
		if err := server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server close error: %v", err)
		}
	}()

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func runWatcher(ctx context.Context, c *catalog, hub *sseHub, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		changed, err := c.refresh()
		if err != nil {
			log.Printf("refresh failed: %v", err)
			continue
		}
		if !changed {
			continue
		}
		m := c.snapshot()
		log.Printf("spec catalog changed: %d specs, rev=%s", len(m.Items), shortSHA(m.Revision))

		data, _ := json.Marshal(map[string]string{
			"revision": m.Revision,
		})
		hub.broadcast(sseEvent{
			Name: "spec-changed",
			Data: string(data),
		})
	}
}

func shortSHA(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

func getEnv(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

const docsPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Local OpenAPI Docs</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css" />
  <style>
    :root {
      color-scheme: light;
      --bg: #f5f7f4;
      --panel: #ffffff;
      --text: #1f2d22;
      --muted: #54635a;
      --line: #d6ddd8;
      --accent: #1a7f4f;
    }
    html, body {
      margin: 0;
      background: linear-gradient(145deg, #eef3ee 0%, #f9fbf8 55%, #eef2ec 100%);
      color: var(--text);
      font-family: "IBM Plex Sans", "Segoe UI", Tahoma, sans-serif;
      min-height: 100%;
    }
    .topbar {
      position: sticky;
      top: 0;
      z-index: 2;
      display: flex;
      gap: 12px;
      align-items: center;
      padding: 10px 14px;
      border-bottom: 1px solid var(--line);
      backdrop-filter: blur(6px);
      background: rgba(245, 247, 244, 0.88);
    }
    .title {
      font-weight: 700;
      letter-spacing: 0.2px;
    }
    .meta {
      margin-left: auto;
      color: var(--muted);
      font-size: 13px;
    }
    .pill {
      display: inline-block;
      border: 1px solid var(--line);
      border-radius: 999px;
      padding: 2px 8px;
      background: var(--panel);
    }
    .warn {
      color: #915b00;
    }
    #swagger-ui {
      max-width: 1200px;
      margin: 0 auto;
      padding-bottom: 28px;
    }
    .empty {
      max-width: 900px;
      margin: 48px auto;
      border: 1px dashed var(--line);
      border-radius: 10px;
      padding: 20px;
      background: var(--panel);
    }
  </style>
</head>
<body>
  <div class="topbar">
    <div class="title">Local OpenAPI Docs</div>
    <div class="pill" id="status">loading...</div>
    <div class="meta" id="meta"></div>
  </div>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-standalone-preset.js"></script>
  <script>
    let ui = null;
    let currentRevision = "";
    let selectedName = "";
    const statusEl = document.getElementById("status");
    const metaEl = document.getElementById("meta");

    function setStatus(text, warn = false) {
      statusEl.textContent = text;
      statusEl.classList.toggle("warn", warn);
    }

    async function fetchManifest() {
      const r = await fetch("/openapi/manifest", { cache: "no-store" });
      if (!r.ok) throw new Error("manifest request failed");
      return r.json();
    }

    function renderEmpty(message) {
      document.getElementById("swagger-ui").innerHTML =
        '<div class="empty">' + message + "</div>";
    }

    function renderSwagger(m) {
      if (!m.items || !m.items.length) {
        renderEmpty("No specs found under ../**/docs/oas/openapi.yaml");
        setStatus("no specs", true);
        metaEl.textContent = "Search root is configured server-side.";
        return;
      }

      const urls = m.items.map((item) => ({ name: item.name, url: item.url }));
      let primary = urls[0].name;
      if (selectedName) {
        const exists = urls.find((u) => u.name === selectedName);
        if (exists) primary = exists.name;
      }
      selectedName = primary;

      document.getElementById("swagger-ui").innerHTML = "";
      ui = SwaggerUIBundle({
        dom_id: "#swagger-ui",
        urls,
        "urls.primaryName": primary,
        deepLinking: true,
        presets: [
          SwaggerUIBundle.presets.apis,
          SwaggerUIStandalonePreset
        ],
        layout: "StandaloneLayout",
        docExpansion: "list",
        persistAuthorization: true
      });

      setStatus("connected");
      metaEl.textContent = "Specs: " + m.items.length + " - rev " + m.revision.slice(0, 8);
    }

    async function rerender() {
      try {
        const m = await fetchManifest();
        currentRevision = m.revision;
        renderSwagger(m);
      } catch (err) {
        setStatus("manifest error", true);
        renderEmpty("Failed to load manifest: " + err.message);
      }
    }

    async function init() {
      await rerender();
      const es = new EventSource("/openapi/events");

      es.addEventListener("ready", () => {
        setStatus("connected");
      });

      es.addEventListener("spec-changed", async (ev) => {
        try {
          const msg = JSON.parse(ev.data);
          if (msg.revision && msg.revision === currentRevision) return;
          await rerender();
          setStatus("updated");
          setTimeout(() => setStatus("connected"), 1200);
        } catch {
          await rerender();
        }
      });

      es.onerror = () => {
        setStatus("reconnecting...", true);
      };
    }

    init();
  </script>
</body>
</html>`
