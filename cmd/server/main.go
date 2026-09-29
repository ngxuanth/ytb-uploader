// Command server is the upload server: a REST API (docs/server-api.md),
// task_mcp / report_mcp for agent sessions, and the WebSocket launchers
// connect to. Uploads wait in a queue per Chrome profile; tasks are saved to
// a JSON file. This file only reads flags and wires the layers together.
package main

import (
	"flag"
	"log"
	"path/filepath"
	"time"

	"github.com/gofiber/fiber/v3"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/adapter/agentws"
	httpapi "gitlab.volio.vn/tech/backend/yt_uploader/internal/adapter/http"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/adapter/jsonstore"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/adapter/taskmcp"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/adapter/taskmcpserver"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/app/taskservice"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/infra/localfs"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8090", "listen address")
	profiles := flag.String("profiles", "profile", "directory whose subfolders are Chrome profiles")
	data := flag.String("data", "data/server/tasks.json", "JSON file the tasks are saved to")
	grace := flag.Duration("finish-grace", 45*time.Second, "after task_finish, stop a session that has not exited within this long (0 = never)")
	recheck := flag.Duration("recheck-processing", 0, "check a video Studio still processes again after this long, up to 6 times (0 = never: only when asked)")
	flag.Parse()
	absProfiles, err := filepath.Abs(*profiles)
	if err != nil {
		log.Fatal(err)
	}
	absData, err := filepath.Abs(*data)
	if err != nil {
		log.Fatal(err)
	}
	cfg := taskservice.DefaultConfig("http://" + *addr)
	cfg.FinishGrace, cfg.Recheck = *grace, *recheck
	app, _, err := newApp(cfg, absProfiles, absData)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on http://%s  ws://%s/ws", *addr, *addr)
	log.Printf("profiles are folders in %s; tasks are saved to %s", absProfiles, absData)
	log.Fatal(app.Listen(*addr))
}

// newApp wires the server: the task service over the JSON store and the
// local disk, served by the HTTP, MCP and WebSocket adapters.
func newApp(cfg taskservice.Config, profilesDir, dataFile string) (*fiber.App, *taskservice.Service, error) {
	svc, err := taskservice.New(cfg, &jsonstore.Store{Path: dataFile}, localfs.Profiles{Dir: profilesDir}, localfs.Files{})
	if err != nil {
		return nil, nil, err
	}
	be := taskmcpserver.Backend(svc)
	app := httpapi.New(svc, httpapi.Mounts{
		TaskMCP:   taskmcp.NewTaskHandler(be),
		ReportMCP: taskmcp.NewReportHandler(be),
		Agents:    agentws.Handler(svc),
	})
	return app, svc, nil
}
