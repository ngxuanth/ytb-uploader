// Package agentws is the server's end of the launcher WebSocket: it turns
// the launcher's messages into task-service calls and gives the service a
// way to send (assign, cancel) back.
package agentws

import (
	"encoding/json"
	"log"

	"github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"

	"gitlab.volio.vn/tech/backend/yt_uploader/internal/app/taskservice"
	"gitlab.volio.vn/tech/backend/yt_uploader/internal/contract"
)

// Handler serves GET /ws.
func Handler(svc *taskservice.Service) fiber.Handler {
	return websocket.New(func(ws *websocket.Conn) { serve(svc, &conn{ws: ws}) })
}

// conn is a taskservice.AgentConn. The service sends with its lock held, so
// writes never interleave.
type conn struct{ ws *websocket.Conn }

func (c *conn) Send(msgType string, data any) error {
	env, err := contract.NewEnvelope(msgType, data)
	if err != nil {
		return err
	}
	return c.ws.WriteJSON(env)
}

// serve reads one launcher's messages. The launcher is subscribed once its
// hello arrives; closing the socket unsubscribes it.
func serve(svc *taskservice.Service, c *conn) {
	defer func() {
		svc.AgentGone(c)
		c.ws.Close()
	}()
	for {
		var env contract.Envelope
		if err := c.ws.ReadJSON(&env); err != nil {
			log.Printf("socket: %v", err)
			return
		}
		svc.AgentSeen(c)
		switch env.Type {
		case contract.MsgHello:
			var hello contract.Hello
			if err := json.Unmarshal(env.Data, &hello); err != nil {
				log.Printf("hello: %v", err)
				continue
			}
			svc.AgentHello(c, hello)
		case contract.MsgReject:
			var rej contract.Reject
			if err := json.Unmarshal(env.Data, &rej); err != nil {
				log.Printf("reject: %v", err)
				continue
			}
			svc.Rejected(rej)
		case contract.MsgEvent:
			var ev contract.Event
			if err := json.Unmarshal(env.Data, &ev); err != nil {
				log.Printf("event: %v", err)
				continue
			}
			if ev.Type == contract.EventSessionEnded {
				svc.SessionEnded(ev)
			} else {
				log.Printf("event %s %s", ev.TaskID, ev.Type)
			}
		case contract.MsgHeartbeat:
			log.Printf("heartbeat %s", env.Data)
		default:
			log.Printf("message %s", env.Type)
		}
	}
}
