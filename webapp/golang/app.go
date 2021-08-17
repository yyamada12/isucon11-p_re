package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/jmoiron/sqlx"
	"github.com/oklog/ulid/v2"
)

var (
	publicDir       string
	fs              http.Handler
	scheduleCounter ScheduleCounter
)

type ScheduleCounter struct {
	m  map[string]int
	mu sync.RWMutex
}

func (sc *ScheduleCounter) Add(key string, val int) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.m[key] += val
}

func (sc *ScheduleCounter) Get(key string) int {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return sc.m[key]
}

type ScheduleCount struct {
	ID    string `db:"id"`
	Count int    `db:"count"`
}

type User struct {
	ID        string    `db:"id" json:"id"`
	Email     string    `db:"email" json:"email"`
	Nickname  string    `db:"nickname" json:"nickname"`
	Staff     bool      `db:"staff" json:"staff"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type Schedule struct {
	ID           string         `db:"id" json:"id"`
	Title        string         `db:"title" json:"title"`
	Capacity     int            `db:"capacity" json:"capacity"`
	Reserved     int            `db:"reserved" json:"reserved"`
	Reservations []*Reservation `db:"reservations" json:"reservations"`
	CreatedAt    time.Time      `db:"created_at" json:"created_at"`
}

type Reservation struct {
	ID         string    `db:"id" json:"id"`
	ScheduleID string    `db:"schedule_id" json:"schedule_id"`
	UserID     string    `db:"user_id" json:"user_id"`
	User       *User     `db:"user" json:"user"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

func getCurrentUser(r *http.Request) *User {
	uidCookie, err := r.Cookie("user_id")
	if err != nil || uidCookie == nil {
		return nil
	}
	row := db.QueryRowxContext(r.Context(), "SELECT * FROM `users` WHERE `id` = ? LIMIT 1", uidCookie.Value)
	user := &User{}
	if err := row.StructScan(user); err != nil {
		return nil
	}
	return user
}

func requiredLogin(w http.ResponseWriter, r *http.Request) bool {
	if getCurrentUser(r) != nil {
		return true
	}
	sendErrorJSON(w, fmt.Errorf("login required"), 401)
	return false
}

func requiredStaffLogin(w http.ResponseWriter, r *http.Request) bool {
	if getCurrentUser(r) != nil && getCurrentUser(r).Staff {
		return true
	}
	sendErrorJSON(w, fmt.Errorf("login required"), 401)
	return false
}

func getReservations(r *http.Request, s *Schedule) error {
	rows, err := db.QueryxContext(r.Context(), "SELECT * FROM `reservations` WHERE `schedule_id` = ?", s.ID)
	if err != nil {
		return err
	}

	defer rows.Close()

	reserved := 0
	s.Reservations = []*Reservation{}

	userIDs := []string{}

	for rows.Next() {
		reservation := &Reservation{}
		if err := rows.StructScan(reservation); err != nil {
			return err
		}
		userIDs = append(userIDs, reservation.UserID)

		s.Reservations = append(s.Reservations, reservation)
		reserved++
	}
	s.Reserved = reserved

	if len(userIDs) == 0 {
		return nil
	}

	var userMap map[string]User
	crtUser := getCurrentUser(r)
	if crtUser != nil && crtUser.Staff {
		userMap = getUserByIDsForStaff(userIDs)
	} else {
		userMap = getUserByIDs(userIDs)
	}

	for _, r := range s.Reservations {
		u := userMap[r.UserID]
		r.User = &u
	}

	return nil
}

func getReservationsCount(r *http.Request, s *Schedule) error {
	rows, err := db.QueryxContext(r.Context(), "SELECT * FROM `reservations` WHERE `schedule_id` = ?", s.ID)
	if err != nil {
		return err
	}

	defer rows.Close()

	reserved := 0
	for rows.Next() {
		reserved++
	}
	s.Reserved = reserved

	return nil
}

func getUser(r *http.Request, id string) *User {
	user := &User{}
	if err := db.QueryRowxContext(r.Context(), "SELECT * FROM `users` WHERE `id` = ? LIMIT 1", id).StructScan(user); err != nil {
		return nil
	}
	if getCurrentUser(r) != nil && !getCurrentUser(r).Staff {
		user.Email = ""
	}
	return user
}

func getUserByIDs(userIDs []string) map[string]User {

	qs, params, err := sqlx.In(`SELECT id, nickname, staff, created_at FROM users WHERE id IN (?)`, userIDs)
	if err != nil {
		log.Fatal(err)
	}

	users := []User{}
	if err := db.Select(&users, qs, params...); err != nil {
		log.Fatal(err)
	}

	res := make(map[string]User)
	for _, user := range users {
		res[user.ID] = user
	}
	return res
}

func getUserByIDsForStaff(userIDs []string) map[string]User {

	qs, params, err := sqlx.In(`SELECT id, email, nickname, staff, created_at FROM users WHERE id IN (?)`, userIDs)
	if err != nil {
		log.Fatal(err)
	}

	users := []User{}
	if err := db.Select(&users, qs, params...); err != nil {
		log.Fatal(err)
	}

	res := make(map[string]User)
	for _, user := range users {
		res[user.ID] = user
	}
	return res
}

func parseForm(r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		return r.ParseForm()
	} else {
		return r.ParseMultipartForm(32 << 20)
	}
}

func serveMux() http.Handler {
	router := mux.NewRouter()

	router.HandleFunc("/initialize", initializeHandler).Methods("POST")
	router.HandleFunc("/api/session", sessionHandler).Methods("GET")
	router.HandleFunc("/api/signup", signupHandler).Methods("POST")
	router.HandleFunc("/api/login", loginHandler).Methods("POST")
	router.HandleFunc("/api/schedules", createScheduleHandler).Methods("POST")
	router.HandleFunc("/api/reservations", createReservationHandler).Methods("POST")
	router.HandleFunc("/api/schedules", schedulesHandler).Methods("GET")
	router.HandleFunc("/api/schedules/{id}", scheduleHandler).Methods("GET")

	dir, err := filepath.Abs(filepath.Join(filepath.Dir(os.Args[0]), "..", "public"))
	if err != nil {
		log.Fatal(err)
	}
	publicDir = dir
	fs = http.FileServer(http.Dir(publicDir))

	router.PathPrefix("/").HandlerFunc(htmlHandler)

	return router
}

func sendJSON(w http.ResponseWriter, data interface{}, statusCode int) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)

	enc := json.NewEncoder(w)
	return enc.Encode(data)
}

func sendErrorJSON(w http.ResponseWriter, err error, statusCode int) error {
	// log.Printf("ERROR: %+v", err)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)

	enc := json.NewEncoder(w)
	return enc.Encode(map[string]string{"error": err.Error()})
}

type initializeResponse struct {
	Language string `json:"language"`
}

func initializeHandler(w http.ResponseWriter, r *http.Request) {
	err := transaction(r.Context(), &sql.TxOptions{}, func(ctx context.Context, tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, "TRUNCATE `reservations`"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "TRUNCATE `schedules`"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "TRUNCATE `users`"); err != nil {
			return err
		}

		id := generateID(tx, "users")
		if _, err := tx.ExecContext(
			ctx,
			"INSERT INTO `users` (`id`, `email`, `nickname`, `staff`, `created_at`) VALUES (?, ?, ?, true, NOW(6))",
			id,
			"isucon2021_prior@isucon.net",
			"isucon",
		); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		sendErrorJSON(w, err, 500)
		return
	}

	scheduleCounts := []ScheduleCount{}
	db.Select(&scheduleCounts, "SELECT schedule_id AS id, count(1) AS count FROM `reservations` GROUP BY `schedule_id`")
	for _, sc := range scheduleCounts {
		scheduleCounter.Add(sc.ID, sc.Count)
	}

	sendJSON(w, initializeResponse{Language: "golang"}, 200)

}

func sessionHandler(w http.ResponseWriter, r *http.Request) {
	sendJSON(w, getCurrentUser(r), 200)
}

func signupHandler(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		sendErrorJSON(w, err, 500)
		return
	}

	user := &User{}

	email := r.FormValue("email")
	nickname := r.FormValue("nickname")
	id := ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
	createdAt := time.Now()

	if _, err := db.Exec(
		"INSERT INTO `users` (`id`, `email`, `nickname`, `created_at`) VALUES (?, ?, ?, ?)",
		id, email, nickname, createdAt,
	); err != nil {
		sendErrorJSON(w, err, 500)
		return
	}
	user.ID = id
	user.Email = email
	user.Nickname = nickname
	user.CreatedAt = createdAt

	sendJSON(w, user, 200)
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		sendErrorJSON(w, err, 500)
		return
	}

	email := r.PostFormValue("email")
	user := &User{}

	if err := db.QueryRowxContext(
		r.Context(),
		"SELECT * FROM `users` WHERE `email` = ? LIMIT 1",
		email,
	).StructScan(user); err != nil {
		sendErrorJSON(w, err, 403)
		return
	}
	cookie := &http.Cookie{
		Name:     "user_id",
		Value:    user.ID,
		Path:     "/",
		MaxAge:   86400,
		HttpOnly: true,
	}
	http.SetCookie(w, cookie)

	sendJSON(w, user, 200)
}

func createScheduleHandler(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		sendErrorJSON(w, err, 500)
		return
	}

	if !requiredStaffLogin(w, r) {
		return
	}

	schedule := &Schedule{}
	id := ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
	title := r.PostFormValue("title")
	capacity, _ := strconv.Atoi(r.PostFormValue("capacity"))
	createdAt := time.Now()

	if _, err := db.Exec(
		"INSERT INTO `schedules` (`id`, `title`, `capacity`, `created_at`) VALUES (?, ?, ?, ?)",
		id, title, capacity, createdAt,
	); err != nil {
		sendErrorJSON(w, err, 500)
		return
	}
	schedule.ID = id
	schedule.Title = title
	schedule.Capacity = capacity
	schedule.CreatedAt = createdAt

	sendJSON(w, schedule, 200)

}

func createReservationHandler(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		sendErrorJSON(w, err, 500)
		return
	}

	currentUser := getCurrentUser(r)
	if currentUser == nil {
		sendErrorJSON(w, fmt.Errorf("login required"), 401)
		return
	}

	reservation := &Reservation{}

	id := ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
	scheduleID := r.PostFormValue("schedule_id")
	userID := currentUser.ID

	rctx := r.Context()

	found := 0
	db.QueryRowContext(rctx, "SELECT 1 FROM `reservations` WHERE `schedule_id` = ? AND `user_id` = ? LIMIT 1", scheduleID, userID).Scan(&found)
	if found == 1 {
		sendErrorJSON(w, fmt.Errorf("already taken"), 403)
		return
	}

	err := transaction(rctx, &sql.TxOptions{}, func(ctx context.Context, tx *sqlx.Tx) error {

		capacity := 0
		if err := tx.QueryRowContext(ctx, "SELECT `capacity` FROM `schedules` WHERE `id` = ? LIMIT 1 FOR UPDATE", scheduleID).Scan(&capacity); capacity == 0 {
			sendErrorJSON(w, fmt.Errorf("schedule not found"), 403)
		} else if err != nil {
			return sendErrorJSON(w, err, 500)
		}

		reserved := scheduleCounter.Get(scheduleID)

		if reserved >= capacity {
			return sendErrorJSON(w, fmt.Errorf("capacity is already full"), 403)
		}

		createdAt := time.Now()
		if _, err := tx.ExecContext(
			ctx,
			"INSERT INTO `reservations` (`id`, `schedule_id`, `user_id`, `created_at`) VALUES (?, ?, ?, ?)",
			id, scheduleID, userID, createdAt,
		); err != nil {
			return err
		}
		scheduleCounter.Add(scheduleID, 1)

		reservation.ID = id
		reservation.ScheduleID = scheduleID
		reservation.UserID = userID
		reservation.CreatedAt = createdAt

		return sendJSON(w, reservation, 200)
	})
	if err != nil {
		sendErrorJSON(w, err, 500)
	}
}

func schedulesHandler(w http.ResponseWriter, r *http.Request) {
	schedules := []*Schedule{}
	rows, err := db.QueryxContext(r.Context(), "SELECT id, title, capacity, created_at FROM `schedules` ORDER BY `id` DESC")
	if err != nil {
		sendErrorJSON(w, err, 500)
		return
	}

	for rows.Next() {
		schedule := &Schedule{}
		if err := rows.StructScan(schedule); err != nil {
			sendErrorJSON(w, err, 500)
			return
		}
		schedule.Reserved = scheduleCounter.Get(schedule.ID)
		schedules = append(schedules, schedule)
	}

	sendJSON(w, schedules, 200)
}

func scheduleHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]

	schedule := &Schedule{}
	if err := db.QueryRowxContext(r.Context(), "SELECT * FROM `schedules` WHERE `id` = ? LIMIT 1", id).StructScan(schedule); err != nil {

		sendErrorJSON(w, err, 500)
		return
	}

	if err := getReservations(r, schedule); err != nil {
		sendErrorJSON(w, err, 500)
		return
	}

	sendJSON(w, schedule, 200)
}

func htmlHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	realpath := filepath.Join(publicDir, path)

	if stat, err := os.Stat(realpath); !os.IsNotExist(err) && !stat.IsDir() {
		fs.ServeHTTP(w, r)
		return
	} else {
		realpath = filepath.Join(publicDir, "index.html")
	}

	file, err := os.Open(realpath)
	if err != nil {
		sendErrorJSON(w, err, 500)
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", "text/html; chartset=utf-8")
	w.WriteHeader(200)
	io.Copy(w, file)
}
