package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
)

var (
	bind string
	port int
)

func init() {
	flag.StringVar(&bind, "bind", "0.0.0.0", "bind address")
	flag.IntVar(&port, "port", 9292, "bind port")

	flag.Parse()
}

func main() {

	scheduleCounter = ScheduleCounter{m: map[string]int{}}
	scheduleCounts := []ScheduleCount{}
	db.Select(&scheduleCounts, "SELECT schedule_id AS id, count(1) AS count FROM `reservations` GROUP BY `schedule_id`")
	for _, sc := range scheduleCounts {
		scheduleCounter.Add(sc.ID, sc.Count)
	}

	usersMap = UsersMap{m: map[string]*User{}}
	users := []User{}
	db.Select(&users, "SELECT * FROM `users`")
	for _, u := range users {
		usersMap.Add(&u)
	}

	usersMapNoEmail = UsersMap{m: map[string]*User{}}
	users = []User{}
	db.Select(&users, "SELECT id, nickname, staff, created_at FROM `users`")
	for _, u := range users {
		usersMapNoEmail.Add(&u)
	}

	srv := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", bind, port),
		Handler: serveMux(),
	}

	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
