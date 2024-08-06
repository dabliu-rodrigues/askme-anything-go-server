package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jsGolden/askme-anything-go-server/internal/store/pgstore"
)

type apiHandler struct {
	q           *pgstore.Queries
	r           *chi.Mux
	upgrader    websocket.Upgrader
	subscribers map[string]map[(*websocket.Conn)]context.CancelFunc
	mu          *sync.Mutex
}

func (h apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.r.ServeHTTP(w, r)
}

func NewHandler(q *pgstore.Queries) http.Handler {
	a := apiHandler{
		q:           q,
		upgrader:    websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }},
		subscribers: make(map[string]map[*websocket.Conn]context.CancelFunc),
		mu:          &sync.Mutex{},
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, middleware.Logger)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Route("/subscribe/{room_id}", func(r chi.Router) {
		r.Use(a.getRoomMiddleware)
		r.Get("/", a.handleSubscribe)
	})

	r.Route("/api", func(r chi.Router) {
		r.Route("/rooms", func(r chi.Router) {
			r.Post("/", a.handleCreateRoom)
			r.Get("/", a.handleGetRooms)

			r.Route("/{room_id}/messages", func(r chi.Router) {
				r.Use(a.getRoomMiddleware)

				r.Get("/", a.handleGetRoomMessages)
				r.Post("/", a.handleCreateRoomMessage)

				r.Route("/{message_id}", func(r chi.Router) {
					r.Use(a.getMessageMiddleware, a.verifyIfMessageIsFromRoomMiddleware)

					r.Get("/", a.handleGetRoomMessage)
					r.Patch("/react", a.handleReactToMessage)
					r.Delete("/react", a.handleRemoveReactFromMessage)
					r.Patch("/answer", a.handleMarkMessageAsAnswered)
				})
			})
		})
	})

	a.r = r
	return a
}

type Message struct {
	Kind   string `json:"kind"`
	Value  any    `json:"value"`
	RoomID string `json:"-"`
}

type MessageMessageCreated struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

const (
	MessageKindMessageCreated = "message_created"
)

func (h apiHandler) notifyClients(msg Message) {
	h.mu.Lock()
	defer h.mu.Unlock()

	subscribers, ok := h.subscribers[msg.RoomID]
	if !ok || len(subscribers) == 0 {
		return
	}

	for conn, cancel := range subscribers {
		if err := conn.WriteJSON(msg); err != nil {
			slog.Error("failed to send message to client", "error", err)
			cancel()
		}
	}
}

type MiddlewareKey string

const (
	roomMiddlewareKey    MiddlewareKey = "room_id"
	messageMiddlewareKey MiddlewareKey = "message_id"
)

func (h apiHandler) getRoomMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawRoomID := chi.URLParam(r, "room_id")
		roomID, err := uuid.Parse(rawRoomID)
		if err != nil {
			http.Error(w, "invalid room id", http.StatusBadRequest)
			return
		}

		room, err := h.q.GetRoom(r.Context(), roomID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "room not found", http.StatusBadRequest)
				return
			}
			http.Error(w, "something went wrong", http.StatusBadRequest)
			return
		}

		ctx := context.WithValue(r.Context(), roomMiddlewareKey, room)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (h apiHandler) getMessageMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawMessageID := chi.URLParam(r, "message_id")
		messageID, err := uuid.Parse(rawMessageID)
		if err != nil {
			http.Error(w, "invalid message id", http.StatusBadRequest)
			return
		}

		message, err := h.q.GetMessage(r.Context(), messageID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "message not found", http.StatusBadRequest)
				return
			}
			http.Error(w, "something went wrong", http.StatusBadRequest)
			return
		}

		ctx := context.WithValue(r.Context(), messageMiddlewareKey, message)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (h apiHandler) verifyIfMessageIsFromRoomMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		room, _ := r.Context().Value(roomMiddlewareKey).(pgstore.Room)
		message, _ := r.Context().Value(messageMiddlewareKey).(pgstore.Message)

		if message.RoomID.String() != room.ID.String() {
			http.Error(w, "message not found", http.StatusNotFound)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (h apiHandler) sendJSONResponse(body interface{}, w http.ResponseWriter) {
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (h apiHandler) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	room, _ := r.Context().Value(roomMiddlewareKey).(pgstore.Room)
	rawRoomID := room.ID.String()

	c, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("failed to upgrade connection", "error", err)
		http.Error(w, "failed to upgrade to websocket connection", http.StatusBadRequest)
		return
	}

	defer c.Close()

	h.mu.Lock()
	ctx, cancel := context.WithCancel(r.Context())

	if _, ok := h.subscribers[rawRoomID]; !ok {
		h.subscribers[rawRoomID] = make(map[*websocket.Conn]context.CancelFunc)
	}
	slog.Info("new client connected", "room_id", rawRoomID, "client_ip", r.RemoteAddr)
	h.subscribers[rawRoomID][c] = cancel
	h.mu.Unlock()
	<-ctx.Done()

	h.mu.Lock()
	delete(h.subscribers[rawRoomID], c)
	h.mu.Unlock()
}

func (h apiHandler) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	type _body struct {
		Theme string `json:"theme"`
	}

	var body _body
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "malformed json", http.StatusBadRequest)
		return
	}

	roomId, err := h.q.InsertRoom(r.Context(), body.Theme)
	if err != nil {
		slog.Error("failed to insert room", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ID string `json:"id"`
	}

	h.sendJSONResponse(response{ID: roomId.String()}, w)
}

func (h apiHandler) handleGetRooms(w http.ResponseWriter, r *http.Request) {
	rooms, err := h.q.GetRooms(r.Context())
	if err != nil {
		slog.Error("failed to list rooms", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	h.sendJSONResponse(rooms, w)
}

func (h apiHandler) handleGetRoomMessages(w http.ResponseWriter, r *http.Request) {
	room, _ := r.Context().Value(roomMiddlewareKey).(pgstore.Room)

	messages, err := h.q.GetRoomMessages(r.Context(), room.ID)
	if err != nil {
		slog.Error("failed to get room messages", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	h.sendJSONResponse(messages, w)
}

func (h apiHandler) handleCreateRoomMessage(w http.ResponseWriter, r *http.Request) {
	room, _ := r.Context().Value(roomMiddlewareKey).(pgstore.Room)

	type _body struct {
		Message string `json:"message"`
	}

	var body _body
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "malformed json", http.StatusBadRequest)
		return
	}

	messageID, err := h.q.InsertMessage(r.Context(), pgstore.InsertMessageParams{
		RoomID:  room.ID,
		Message: body.Message,
	})

	if err != nil {
		slog.Error("failed to insert message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ID string `json:"id"`
	}

	h.sendJSONResponse(response{ID: messageID.String()}, w)

	go h.notifyClients(Message{
		Kind:   MessageKindMessageCreated,
		RoomID: room.ID.String(),
		Value: MessageMessageCreated{
			ID:      messageID.String(),
			Message: body.Message,
		},
	})
}

func (h apiHandler) handleGetRoomMessage(w http.ResponseWriter, r *http.Request) {
	message, _ := r.Context().Value(messageMiddlewareKey).(pgstore.Message)
	h.sendJSONResponse(message, w)
}

func (h apiHandler) handleReactToMessage(w http.ResponseWriter, r *http.Request) {
	message, _ := r.Context().Value(messageMiddlewareKey).(pgstore.Message)

	rc, err := h.q.ReactToMessage(r.Context(), message.ID)
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ReactionCount int64 `json:"reaction_count"`
	}
	h.sendJSONResponse(response{ReactionCount: rc}, w)
}

func (h apiHandler) handleRemoveReactFromMessage(w http.ResponseWriter, r *http.Request) {
	message, _ := r.Context().Value(messageMiddlewareKey).(pgstore.Message)

	if message.ReactionCount <= 0 {
		http.Error(w, "there's no react to remove", http.StatusBadRequest)
		return
	}

	rc, err := h.q.ReactToMessage(r.Context(), message.ID)
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ReactionCount int64 `json:"reaction_count"`
	}
	h.sendJSONResponse(response{ReactionCount: rc}, w)
}

func (h apiHandler) handleMarkMessageAsAnswered(w http.ResponseWriter, r *http.Request) {
	message, _ := r.Context().Value(messageMiddlewareKey).(pgstore.Message)

	if !message.Answered {
		if err := h.q.MarkMessageAsAnswered(r.Context(), message.ID); err != nil {
			http.Error(w, "something went wrong", http.StatusInternalServerError)
			return
		}
	}
}
