// Package http is the server's REST API (Fiber). Handlers only parse the
// request, call the task service and map its errors to status codes; see
// docs/server-api.md for the API itself.
package http

import (
	"errors"
	nethttp "net/http"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/app/taskservice"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

// Handlers the app mounts next to the REST routes.
type Mounts struct {
	TaskMCP   nethttp.Handler // /task/mcp
	ReportMCP nethttp.Handler // /report/mcp
	Agents    fiber.Handler   // /ws
}

// New builds the server's Fiber app.
func New(svc *taskservice.Service, m Mounts) *fiber.App {
	app := fiber.New()
	h := handlers{svc}
	taskH := adaptor.HTTPHandler(m.TaskMCP)
	reportH := adaptor.HTTPHandler(m.ReportMCP)
	app.All("/task/mcp", taskH)
	app.All("/task/mcp/*", taskH)
	app.All("/report/mcp", reportH)
	app.All("/report/mcp/*", reportH)
	app.Get("/profiles", h.profiles)
	app.Get("/agent", h.agents)
	app.Post("/uploads", h.upload)
	app.Get("/queues", h.queues)
	app.Get("/tasks", h.listTasks)
	app.Get("/tasks/:id", h.getTask)
	app.Get("/tasks/:id/video", h.getVideo)
	app.Post("/tasks/:id/video/check", h.checkVideo)
	app.Get("/videos", h.listVideos)
	app.Post("/tasks/:id/retry", h.retry)
	app.Post("/tasks/:id/cancel", h.cancel)
	app.Get("/files/:id/:name", h.file)
	app.Get("/ws", m.Agents)
	return app
}

type handlers struct{ svc *taskservice.Service }

// fail writes err with the status its kind calls for.
func fail(c fiber.Ctx, err error) error {
	body := fiber.Map{"error": err.Error()}
	status := fiber.StatusInternalServerError
	switch taskservice.KindOf(err) {
	case taskservice.Invalid:
		status = fiber.StatusBadRequest
		var e *taskservice.Error
		if errors.As(err, &e) && e.Known != nil {
			body["profiles"] = e.Known
		}
	case taskservice.NotFound:
		status = fiber.StatusNotFound
	case taskservice.Conflict:
		status = fiber.StatusConflict
	case taskservice.Unauthorized:
		status = fiber.StatusUnauthorized
	}
	return c.Status(status).JSON(body)
}

func (h handlers) profiles(c fiber.Ctx) error {
	names, err := h.svc.Profiles()
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(fiber.Map{"profiles": names})
}

// agents lists the connected launchers; "agent" is the newest one.
func (h handlers) agents(c fiber.Ctx) error {
	agents := h.svc.Agents()
	var newest *taskservice.AgentInfo
	if len(agents) > 0 {
		newest = &agents[0]
	}
	return c.JSON(fiber.Map{"connected": len(agents) > 0, "agent": newest, "agents": agents})
}

func (h handlers) upload(c fiber.Ctx) error {
	var in taskservice.UploadInput
	if err := c.Bind().JSON(&in); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	v, err := h.svc.Upload(in)
	if err != nil {
		return fail(c, err)
	}
	return c.Status(fiber.StatusAccepted).JSON(v)
}

func (h handlers) queues(c fiber.Ctx) error {
	return c.JSON(fiber.Map{"queues": h.svc.Queues()})
}

// listTasks: newest first; ?status=FAILED,LOST and ?profile=NAME filter.
func (h handlers) listTasks(c fiber.Ctx) error {
	f := taskservice.TaskFilter{Profile: c.Query("profile")}
	for _, s := range strings.Split(c.Query("status"), ",") {
		if s = strings.ToUpper(strings.TrimSpace(s)); s != "" {
			f.Statuses = append(f.Statuses, contract.Status(s))
		}
	}
	return c.JSON(fiber.Map{"tasks": h.svc.ListTasks(f)})
}

func (h handlers) getTask(c fiber.Ctx) error {
	v, err := h.svc.Task(c.Params("id"))
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(v)
}

func (h handlers) retry(c fiber.Ctx) error {
	front := c.Query("front") == "true" || c.Query("front") == "1"
	v, err := h.svc.Retry(c.Params("id"), front)
	if err != nil {
		return fail(c, err)
	}
	return c.Status(fiber.StatusAccepted).JSON(v)
}

func (h handlers) cancel(c fiber.Ctx) error {
	v, err := h.svc.Cancel(c.Params("id"))
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(v)
}

func (h handlers) getVideo(c fiber.Ctx) error {
	v, err := h.svc.Video(c.Params("id"))
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(v)
}

func (h handlers) checkVideo(c fiber.Ctx) error {
	v, err := h.svc.CheckVideo(c.Params("id"))
	if err != nil {
		return fail(c, err)
	}
	return c.Status(fiber.StatusAccepted).JSON(v)
}

func (h handlers) listVideos(c fiber.Ctx) error {
	return c.JSON(fiber.Map{"videos": h.svc.Videos(c.Query("profile"))})
}

// file serves a task's video or thumbnail to the launcher.
func (h handlers) file(c fiber.Ctx) error {
	path, err := h.svc.File(c.Params("id"), c.Params("name"))
	if err != nil {
		return fiber.ErrNotFound
	}
	return c.SendFile(path)
}
