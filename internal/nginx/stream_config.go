package nginx

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"text/template"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
)

func (m *Manager) RebuildStreamConfig(port int, streams []models.Stream) error {
	return m.rebuildStreamConfig(port, streams, true)
}

func (m *Manager) RebuildStreamConfigNoReload(port int, streams []models.Stream) error {
	return m.rebuildStreamConfig(port, streams, false)
}

func (m *Manager) rebuildStreamConfig(port int, streams []models.Stream, reload bool) error {
	if len(streams) == 0 {
		return m.deleteStreamConfig(port, reload)
	}

	useSNI := len(streams) > 1 || streams[0].Domain != ""
	var buf bytes.Buffer

	if !useSNI {
		s := streams[0]
		proto := ""
		if s.Protocol == "udp" {
			proto = " udp"
		}
		mapName := fmt.Sprintf("stream_simple_map_%d", s.ListenPort)
		tmpl := `
map $remote_addr ${{ .MapName }} {
    default {{ .Upstream }};
}

server {
    listen {{ .ListenPort }}{{ .Proto }};
    listen [::]:{{ .ListenPort }}{{ .Proto }};
    proxy_pass ${{ .MapName }};
}
`
		data := struct {
			ListenPort int
			Proto      string
			Upstream   string
			MapName    string
		}{ListenPort: s.ListenPort, Proto: proto, Upstream: s.Upstream, MapName: mapName}

		t, _ := template.New("simple_stream").Parse(tmpl)
		if err := t.Execute(&buf, data); err != nil {
			return err
		}
	} else {
		mapName := fmt.Sprintf("stream_map_%d", port)
		buf.WriteString(fmt.Sprintf("map $ssl_preread_server_name $%s {\n", mapName))
		for _, s := range streams {
			if s.Domain != "" {
				buf.WriteString(fmt.Sprintf("    %s %s;\n", s.Domain, s.Upstream))
			}
		}
		var defaultStream *models.Stream
		for _, s := range streams {
			if s.Domain == "" {
				defaultStream = &s
				break
			}
		}
		if defaultStream != nil {
			buf.WriteString(fmt.Sprintf("    default %s;\n", defaultStream.Upstream))
		}
		buf.WriteString("}\n\n")
		buf.WriteString("server {\n")
		buf.WriteString(fmt.Sprintf("    listen %d;\n", port))
		buf.WriteString("    ssl_preread on;\n")
		buf.WriteString(fmt.Sprintf("    proxy_pass $%s;\n", mapName))
		buf.WriteString("}\n")
	}

	configFile := filepath.Join(m.StreamsDir, fmt.Sprintf("port_%d.conf", port))
	if err := os.WriteFile(configFile, buf.Bytes(), 0644); err != nil {
		return err
	}
	slog.Info("Rebuilt stream config", "port", port, "file", configFile)
	if reload {
		return m.Reload()
	}
	return nil
}

func (m *Manager) DeleteStreamConfig(port int) error {
	return m.deleteStreamConfig(port, true)
}

func (m *Manager) DeleteStreamConfigNoReload(port int) error {
	return m.deleteStreamConfig(port, false)
}

func (m *Manager) deleteStreamConfig(port int, reload bool) error {
	target := filepath.Join(m.StreamsDir, fmt.Sprintf("port_%d.conf", port))
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	slog.Info("Deleted stream config", "port", port, "file", target)
	if reload {
		return m.Reload()
	}
	return nil
}
