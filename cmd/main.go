package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"sre-lab/internal/db"
	"sre-lab/internal/redis"

	"strconv"
	"time"

	redisPkg "github.com/redis/go-redis/v9"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type createTaskReq struct {
	Title string `json:"title"`
}

type updateTaskReq struct {
	Title string `json:"title"`
	Done  bool   `json:"done"`
}

type Task struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Done      bool      `json:"done"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type contextKey string

const requestIDKey contextKey = "request_id"

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(data)
}

func newRequestID() string {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}

	return fmt.Sprintf("%x", b[:])
}

func getRequestID(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey).(string)
	return requestID
}

func writeJSON(w http.ResponseWriter, status int, data any, success bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"data":      data,
		"success":   success,
		"timestamp": time.Now(),
	})
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = newRequestID()
		}

		w.Header().Set("X-Request-ID", requestID)

		ctx := context.WithValue(r.Context(), requestIDKey, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func loggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(recorder, r)

			logger.Info(
				"http_request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", recorder.status),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
				slog.String("remote_addr", r.RemoteAddr),
				slog.String("user_agent", r.UserAgent()),
				slog.String("request_id", getRequestID(r.Context())),
			)
		})
	}
}

func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()

		httpRequestFlight.Inc()
		defer httpRequestFlight.Dec()

		recorder := &statusRecorder{
			ResponseWriter: w,
			status:         http.StatusOK,
		}

		next.ServeHTTP(recorder, r)
		status := strconv.Itoa(recorder.status)
		path := r.URL.Path
		duration := time.Since(start).Seconds()

		httpRequestTotal.WithLabelValues(r.Method, path, status).Inc()
		httpRequestDuration.WithLabelValues(r.Method, path, status).Observe(duration)
	})
}

func main() {
	prometheus.MustRegister(httpRequestTotal)
	prometheus.MustRegister(httpRequestDuration)
	prometheus.MustRegister(httpRequestFlight)
	_ = godotenv.Load()

	dbDsn := os.Getenv("DATABASE_URL")
	redisDsn := os.Getenv("REDIS_ADDR")

	if dbDsn == "" {
		log.Fatalln("database dsn string not write in env")
	}
	if redisDsn == "" {
		log.Println("redis dsn string not write in env")
	}

	ctx := context.Background()

	pool, err := db.InitDB(ctx, dbDsn)

	if err != nil {
		log.Fatalf("pgx pool connect failed error: %v", err)
	}
	defer pool.Close()

	log.Println("connected to postgres")

	redisClient, err := redis.InitRedis(ctx, redisDsn)
	if err != nil {
		log.Printf("redis client connection error: %v", err)
	}
	defer redisClient.Close()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	app := chi.NewRouter()
	app.Use(requestIDMiddleware)
	app.Use(loggingMiddleware(logger))
	app.Use(metricsMiddleware)

	app.Handle("/metrics", promhttp.Handler())

	app.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true}, true)
	})

	app.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		appCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := pool.Ping(appCtx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, "postgres is not ready", false)
			return
		}

		if err := redisClient.Ping(appCtx).Err(); err != nil {
			writeJSON(w, http.StatusOK, "postgres is ready but redis is down", false)
			return
		}

		writeJSON(w, http.StatusOK, "db and redis are ready", true)
	})

	app.Post("/api/tasks", func(w http.ResponseWriter, r *http.Request) {
		var req createTaskReq

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, fmt.Sprintf("parsing task req body error: %w", err), false)
			return
		}

		if req.Title == "" {
			writeJSON(w, http.StatusBadRequest, "title is required", false)
			return
		}

		appCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)

		defer cancel()

		var task Task

		err := pool.QueryRow(
			appCtx,
			`
			INSERT INTO tasks (title) 
			VALUES ($1) 
			RETURNING id, title, done, created_at, updated_at
			`,
			req.Title,
		).Scan(
			&task.ID,
			&task.Title,
			&task.Done,
			&task.CreatedAt,
			&task.UpdatedAt,
		)

		if err != nil {
			writeJSON(w, http.StatusInternalServerError, fmt.Sprintf("insert task error: %w", err), false)
			return
		}

		taskByte, err := json.Marshal(task)

		if err != nil {
			writeJSON(w, http.StatusInternalServerError, fmt.Sprintf("parse task to json string error: %w", err), false)
			return
		}

		if err := redisClient.Set(appCtx, fmt.Sprintf("task:%d", task.ID), string(taskByte), 1*time.Hour).Err(); err != nil {
			log.Printf("cache set task id %d error: %v", task.ID, err)
		}

		writeJSON(w, http.StatusCreated, task, true)
	})

	app.Get("/api/tasks", func(w http.ResponseWriter, r *http.Request) {
		var tasks []Task

		appCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		rows, err := pool.Query(appCtx, `
		  SELECT id, title, done, created_at, updated_at 
		  FROM tasks 
		  ORDER BY id DESC
		`)

		if err != nil {
			writeJSON(w, http.StatusInternalServerError, fmt.Sprintf("list tasks error: %w", err), false)
			return
		}
		defer rows.Close()

		for rows.Next() {
			var task Task

			if err := rows.Scan(
				&task.ID,
				&task.Title,
				&task.Done,
				&task.CreatedAt,
				&task.UpdatedAt,
			); err != nil {
				writeJSON(w, http.StatusInternalServerError, fmt.Sprintf("scan rows for task error: %w", err), false)
				return
			}

			tasks = append(tasks, task)
		}

		if err := rows.Err(); err != nil {
			writeJSON(w, http.StatusInternalServerError, fmt.Sprintf("rows error of tasks: %w", err), false)
			return
		}

		writeJSON(w, http.StatusOK, tasks, true)
	})

	app.Get("/api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		strId := chi.URLParam(r, "id")

		id, err := strconv.Atoi(strId)

		if err != nil {
			writeJSON(w, http.StatusBadRequest, fmt.Sprintf("task id parsing error: %v", err), false)
			return
		}

		appCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		var task Task

		val, err := redisClient.Get(appCtx, fmt.Sprintf("task:%d", id)).Result()

		if err == redisPkg.Nil {
			err = pool.QueryRow(appCtx, `
			SELECT id, title, done, created_at, updated_at 
			FROM tasks 
			WHERE id = $1
			`, id).Scan(
				&task.ID,
				&task.Title,
				&task.Done,
				&task.CreatedAt,
				&task.UpdatedAt,
			)

			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					writeJSON(w, http.StatusNotFound, fmt.Sprintf("task id %d not found", id), false)
					return
				}
				writeJSON(w, http.StatusInternalServerError, fmt.Sprintf("get task by id: %d error: %v", id, err), false)
				return
			}

			log.Printf("query task id %d by db", id)

			taskByte, err := json.Marshal(task)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, fmt.Sprintf("parse task to json string error: %v", err), false)
				return
			}

			if err := redisClient.Set(appCtx, fmt.Sprintf("task:%d", task.ID), string(taskByte), 1*time.Hour).Err(); err != nil {
				log.Printf("cache set task id %d error: %v", task.ID, err)
			}

			writeJSON(w, http.StatusOK, task, true)
			return
		}

		if err != nil {
			log.Printf("cache get task id %d error: %v", id, err)

			err = pool.QueryRow(appCtx, `
			SELECT id, title, done, created_at, updated_at 
			FROM tasks 
			WHERE id = $1
			`, id).Scan(
				&task.ID,
				&task.Title,
				&task.Done,
				&task.CreatedAt,
				&task.UpdatedAt,
			)

			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					writeJSON(w, http.StatusNotFound, fmt.Sprintf("task id %d not found", id), false)
					return
				}
				writeJSON(w, http.StatusInternalServerError, fmt.Sprintf("get task by id: %d error: %v", id, err), false)
				return
			}

			writeJSON(w, http.StatusOK, task, true)
			return
		}

		err = json.Unmarshal([]byte(val), &task)

		if err != nil {
			writeJSON(w, http.StatusInternalServerError, fmt.Sprintf("parse redis json error: %w", err), false)
			return
		}

		log.Printf("query task id %d by redis", id)

		writeJSON(w, http.StatusOK, task, true)
	})

	app.Put("/api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		appCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		strId := chi.URLParam(r, "id")

		id, err := strconv.Atoi(strId)

		if err != nil {
			writeJSON(w, http.StatusBadRequest, fmt.Sprintf("update task id %s error: %v", strId, err), false)
			return
		}

		var req updateTaskReq

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, fmt.Sprintf("parsing update task error: %v", err), false)
			return
		}

		if req.Title == "" {
			writeJSON(w, http.StatusBadRequest, "title is required", false)
			return
		}

		var updatedTask Task

		err = pool.QueryRow(appCtx, `
		UPDATE tasks 
		SET title = $1, 
		    done = $2,
		    updated_at = now() 
		WHERE id = $3 
		RETURNING id, title, done, created_at, updated_at
		`, req.Title, req.Done, id).Scan(
			&updatedTask.ID,
			&updatedTask.Title,
			&updatedTask.Done,
			&updatedTask.CreatedAt,
			&updatedTask.UpdatedAt,
		)

		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, http.StatusNotFound, fmt.Sprintf("task id %d not found", id), false)
				return
			}
			writeJSON(w, http.StatusInternalServerError, fmt.Sprintf("update task by id %d error: %v", id, err), false)
			return
		}

		if err := redisClient.Del(appCtx, fmt.Sprintf("task:%d", id)).Err(); err != nil {
			log.Printf("cache delete task id %d error: %v", id, err)
		}

		writeJSON(w, http.StatusOK, updatedTask, true)
	})

	app.Delete("/api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		appCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		strId := chi.URLParam(r, "id")

		id, err := strconv.Atoi(strId)

		if err != nil {
			writeJSON(w, http.StatusBadRequest, fmt.Sprintf("parsing request id error: %v", err), false)
			return
		}

		tag, err := pool.Exec(appCtx, `
		DELETE FROM tasks WHERE id = $1
		`, id)

		if err != nil {
			writeJSON(w, http.StatusInternalServerError, fmt.Sprintf("delete task id %d error: %v", id, err), false)
			return
		}

		if tag.RowsAffected() == 0 {
			writeJSON(w, http.StatusNotFound, fmt.Sprintf("task id %d not found", id), false)
			return
		}

		if err := redisClient.Del(appCtx, fmt.Sprintf("task:%d", id)).Err(); err != nil {
			log.Printf("cache delete task id %d error: %v", id, err)
		}

		writeJSON(w, http.StatusOK, fmt.Sprintf("deleted task id %d success", id), true)
	})

	log.Println("server listen to port: 8080")

	log.Println(http.ListenAndServe(":8080", app))
}
