package local

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"tailscale.com/client/local"

	"mkmba.nz/tsserve/proxy"
)

//go:embed status.html
var statusHTML string

var statusTmpl = template.Must(template.New("status").Parse(statusHTML))

type statusData struct {
	Hostname       string
	Uptime         string
	Now            string
	DiscoveryMode  string
	MetricsAddr    string
	TraefikEnabled bool
	TraefikPort    int
	Tailnet        tailnetView
	Services       []serviceRow
	Build          buildView
}

type tailnetView struct {
	Available      bool
	Error          string
	Hostname       string
	FQDN           string
	TailnetName    string
	MagicDNSSuffix string
	IPs            []string
	BackendState   string
}

type serviceRow struct {
	Service        string
	Backend        string
	Caps           []string
	ContainerShort string
	RegisteredAgo  string
}

type buildView struct {
	Version   string
	Revision  string
	GoVersion string
}

// statusHandler renders the status page. discoveryMode is captured once at
// server-construction time (env var, set at startup).
func (s *Server) statusHandler() http.HandlerFunc {
	build := readBuild()
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		data := statusData{
			Uptime:         humanDuration(time.Since(s.Started)),
			Now:            time.Now().UTC().Format(time.RFC3339),
			DiscoveryMode:  s.DiscoveryMode,
			MetricsAddr:    s.MetricsAddr,
			TraefikEnabled: s.TraefikPort > 0,
			TraefikPort:    s.TraefikPort,
			Tailnet:        readTailnet(ctx, s.LocalClient),
			Services:       rowsFor(s.Manager.Snapshot()),
			Build:          build,
		}
		data.Hostname = data.Tailnet.Hostname
		if data.Hostname == "" {
			data.Hostname = "tsserve"
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := statusTmpl.Execute(w, data); err != nil {
			s.Logger.Warn("status template execute failed", "err", err)
		}
	}
}

func readTailnet(ctx context.Context, lc *local.Client) tailnetView {
	if lc == nil {
		return tailnetView{Error: "no local client"}
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return tailnetView{Error: err.Error()}
	}
	v := tailnetView{Available: true, BackendState: st.BackendState}
	if st.Self != nil {
		v.Hostname = st.Self.HostName
		v.FQDN = strings.TrimSuffix(st.Self.DNSName, ".")
		for _, ip := range st.Self.TailscaleIPs {
			v.IPs = append(v.IPs, ip.String())
		}
	}
	if st.CurrentTailnet != nil {
		v.TailnetName = st.CurrentTailnet.Name
		v.MagicDNSSuffix = st.CurrentTailnet.MagicDNSSuffix
	}
	return v
}

func rowsFor(services []proxy.ServiceView) []serviceRow {
	out := make([]serviceRow, 0, len(services))
	now := time.Now()
	for _, s := range services {
		out = append(out, serviceRow{
			Service:        s.Service,
			Backend:        s.Backend,
			Caps:           s.Caps,
			ContainerShort: shortID(s.ContainerID),
			RegisteredAgo:  humanDuration(now.Sub(s.RegisteredAt)) + " ago",
		})
	}
	return out
}

func shortID(id string) string {
	// Strip ARN-style path prefix so ECS task ARNs surface their task UUID
	// rather than the constant "arn:aws:ecs:" prefix. Docker container IDs
	// have no '/' and are unaffected.
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func humanDuration(d time.Duration) string {
	if d < time.Second {
		return "just now"
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	return fmt.Sprintf("%dd%dh", days, hours)
}

func readBuild() buildView {
	v := buildView{Version: "unknown", Revision: "unknown", GoVersion: runtime.Version()}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return v
	}
	if bi.Main.Version != "" {
		v.Version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			v.Revision = s.Value
			if len(v.Revision) > 12 {
				v.Revision = v.Revision[:12]
			}
		}
	}
	return v
}
