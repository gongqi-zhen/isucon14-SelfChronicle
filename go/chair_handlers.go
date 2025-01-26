package main

import (
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/goccy/go-json"
	"github.com/oklog/ulid/v2"
)

type chairPostChairsRequest struct {
	Name               string `json:"name"`
	Model              string `json:"model"`
	ChairRegisterToken string `json:"chair_register_token"`
}

type chairPostChairsResponse struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id"`
}

func chairPostChairs(w http.ResponseWriter, r *http.Request) {
	req := &chairPostChairsRequest{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Model == "" || req.ChairRegisterToken == "" {
		writeError(w, http.StatusBadRequest, errors.New("some of required fields(name, model, chair_register_token) are empty"))
		return
	}

	owner := &Owner{}
	if err := db2.Get(owner, "SELECT * FROM owners WHERE chair_register_token = ?", req.ChairRegisterToken); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, errors.New("invalid chair_register_token"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	chairID := ulid.Make().String()
	accessToken := secureRandomStr(32)

	_, err := db.Exec(
		"INSERT INTO chairs (id, owner_id, name, model, is_active, access_token) VALUES (?, ?, ?, ?, ?, ?)",
		chairID, owner.ID, req.Name, req.Model, false, accessToken,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Path:  "/",
		Name:  "chair_session",
		Value: accessToken,
	})

	writeJSON(w, http.StatusCreated, &chairPostChairsResponse{
		ID:      chairID,
		OwnerID: owner.ID,
	})
}

type postChairActivityRequest struct {
	IsActive bool `json:"is_active"`
}

func chairPostActivity(w http.ResponseWriter, r *http.Request) {
	chair := r.Context().Value("chair").(*Chair)

	req := &postChairActivityRequest{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	_, err := db.Exec("UPDATE chairs SET is_active = ? WHERE id = ?", req.IsActive, chair.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type chairPostCoordinateResponse struct {
	RecordedAt int64 `json:"recorded_at"`
}

type updateChairLocationsRequest struct {
	ID        string
	Latitude  int
	Longitude int
	Distance  int
	Now       time.Time
}

var updateChairLocationsChs = sync.Map{}
var updateChairLocationsWorkers = 0

func chairLocationsUpdateWorker(n int) {
	ch := make(chan updateChairLocationsRequest)
	updateChairLocationsChs.Store(n, ch)

WORKER:
	for {
		reqs := make([]updateChairLocationsRequest, 0, 100)
		timer := time.NewTimer(100 * time.Millisecond)
	WAIT:
		for {
			select {
			case <-timer.C:
				break WAIT
			case r := <-ch:
				reqs = append(reqs, r)
			}
			if len(reqs) >= 100 {
				break WAIT
			}
		}
		if len(reqs) == 0 {
			continue WORKER
		}
		tx, _ := db.Beginx()
		slog.Info("updating chair locations", "worker", n, "count", len(reqs))
		for _, req := range reqs {
			if _, err := db.Exec(
				`UPDATE chairs SET latitude = ?, longitude = ?, total_distance = total_distance + ?, moved_at = ?, updated_at = updated_at WHERE id = ?`,
				req.Latitude, req.Longitude, req.Distance, req.Now, req.ID,
			); err != nil {
				slog.Error("failed to update chair location", "id", req.ID, "err", err)
				tx.Rollback()
				continue WORKER
			}
		}
		tx.Commit()
	}
}

// ULIDを整数に変換してnで割った剰余を計算する
func ulidMod(s string, n int) int {
	id, err := ulid.ParseStrict(s)
	if err != nil {
		panic("not a valid ulid")
	}
	hasher := fnv.New64a()
	_, err = hasher.Write(id[:])
	if err != nil {
		panic("failed to hash")
	}
	return int(hasher.Sum64() % uint64(n))
}

func chairPostCoordinate(w http.ResponseWriter, r *http.Request) {
	req := &Coordinate{}
	if err := bindJSON(r, req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	chair := r.Context().Value("chair").(*Chair)

	now := time.Now()
	distance := 0
	if chair.Latitude != nil && chair.Longitude != nil {
		distance = calculateDistance(*chair.Latitude, *chair.Longitude, req.Latitude, req.Longitude)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	ch := make(chan *Ride, 1)
	go func() {
		defer wg.Done()

		ride := &Ride{}
		if r, ok := chairsInRide.Load(chair.ID); ok {
			ride = r.(*Ride)
		} else if err := db.Get(ride, `SELECT * FROM rides WHERE chair_id = ? ORDER BY updated_at DESC LIMIT 1`, chair.ID); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				slog.Error("failed to get ride", "err", err)
				return
			}
		}
		ch <- ride
		if _, err := db.Exec(
			`UPDATE chairs SET latitude = ?, longitude = ?, total_distance = total_distance + ?, moved_at = ?, updated_at = updated_at WHERE id = ?`,
			req.Latitude, req.Longitude, distance, now, chair.ID,
		); err != nil {
			slog.Error("failed to update chair location", "id", chair.ID, "err", err)
			return
		}
	}()

	tx2, err := db2.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx2.Rollback()
	ride := <-ch
	newStatus := ""
	if ride.ID != "" {
		status, err := getLatestRideStatus(tx2, ride.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if status != "COMPLETED" && status != "CANCELED" {
			if req.Latitude == ride.PickupLatitude && req.Longitude == ride.PickupLongitude && status == "ENROUTE" {
				// rideStatusCache.Store(ride.ID, rideStatus{Status: "PICKUP", UpdatedAt: time.Now()})
				tx2.Exec("UPDATE ride_status SET status = 'PICKUP' WHERE ride_id = ?", ride.ID)
				newStatus = "PICKUP"
			}
			if req.Latitude == ride.DestinationLatitude && req.Longitude == ride.DestinationLongitude && status == "CARRYING" {
				// rideStatusCache.Store(ride.ID, rideStatus{Status: "ARRIVED", UpdatedAt: time.Now()})
				tx2.Exec("UPDATE ride_status SET status = 'ARRIVED' WHERE ride_id = ?", ride.ID)
				newStatus = "ARRIVED"
			}
		}
	}
	if err := tx2.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if newStatus != "" {
		go sendNotificationSSE(chair.ID, ride, newStatus)
		go sendNotificationSSEApp(ride.UserID, ride, newStatus)
	}
	wg.Wait()

	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	io.WriteString(w, `{"recorded_at":`+fmt.Sprint(now.UnixMilli())+`}`)
}

type simpleUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type chairGetNotificationResponse struct {
	Data *chairGetNotificationResponseData `json:"data"`
}

type chairGetNotificationResponseData struct {
	RideID                string     `json:"ride_id"`
	User                  simpleUser `json:"user"`
	PickupCoordinate      Coordinate `json:"pickup_coordinate"`
	DestinationCoordinate Coordinate `json:"destination_coordinate"`
	Status                string     `json:"status"`
}

type postChairRidesRideIDStatusRequest struct {
	Status string `json:"status"`
}

func chairPostRideStatus(w http.ResponseWriter, r *http.Request) {
	rideID := r.PathValue("ride_id")

	chair := r.Context().Value("chair").(*Chair)

	req := &postChairRidesRideIDStatusRequest{}
	if err := bindJSON(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	ride := &Ride{}
	if err := db.Get(ride, "SELECT * FROM rides WHERE id = ?", rideID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("ride not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if ride.ChairID.String != chair.ID {
		writeError(w, http.StatusBadRequest, errors.New("not assigned to this ride"))
		return
	}

	newStatus := ""
	switch req.Status {
	// Acknowledge the ride
	case "ENROUTE":
		// rideStatusCache.Store(ride.ID, rideStatus{Status: "ENROUTE", UpdatedAt: time.Now()})
		db2.Exec("UPDATE ride_status SET status = 'ENROUTE' WHERE ride_id = ?", ride.ID)
		newStatus = "ENROUTE"
	// After Picking up user
	case "CARRYING":
		status, err := getLatestRideStatus(db2, ride.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if status != "PICKUP" {
			writeError(w, http.StatusBadRequest, errors.New("chair has not arrived yet"))
			return
		}
		// rideStatusCache.Store(ride.ID, rideStatus{Status: "CARRYING", UpdatedAt: time.Now()})
		db2.Exec("UPDATE ride_status SET status = 'CARRYING' WHERE ride_id = ?", ride.ID)
		newStatus = "CARRYING"
	default:
		writeError(w, http.StatusBadRequest, errors.New("invalid status"))
	}

	if newStatus != "" {
		sendNotificationSSE(chair.ID, ride, req.Status)
		sendNotificationSSEApp(ride.UserID, ride, req.Status)
	}

	w.WriteHeader(http.StatusNoContent)
}

var chairChannels = sync.Map{}

type notify struct {
	Ride   *Ride
	Status string
}

var usersMinimalCache = sync.Map{}

const chanSize = 60

func sendNotificationSSE(chairID string, ride *Ride, status string) {
	if chairID == "" {
		panic("chairID is empty")
	}
	if ride == nil {
		panic("ride is nil")
	}
	if status == "" {
		panic("status is empty")
	}
	_ch, _ := chairChannels.LoadOrStore(chairID, make(chan notify, chanSize))
	ch := _ch.(chan notify)
	select {
	case ch <- notify{Ride: ride, Status: status}:
	default:
		log.Println("dropped notification", chairID, ride.ID, status)
		// non-blocking
	}
}

func chairGetNotificationSSE(w http.ResponseWriter, r *http.Request) {
	chair := r.Context().Value("chair").(*Chair)

	_ch, _ := chairChannels.LoadOrStore(chair.ID, make(chan notify, chanSize))
	ch := _ch.(chan notify)

	// Server Sent Events
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	var lastRide *Ride
	var lastRideStatus string
	f := func() (respond bool, err error) {
		slog.Debug("waiting", "chair", chair.ID)
		n := <-ch
		slog.Debug("received", "notification", n)
		ride := n.Ride
		status := n.Status

		if lastRide != nil && ride.ID == lastRide.ID && status == lastRideStatus {
			return false, nil
		}

		user := &User{}
		if u, ok := usersMinimalCache.Load(ride.UserID); ok {
			user = u.(*User)
		} else {
			if err := db.Get(user, "SELECT id, firstname, lastname FROM users WHERE id = ?", ride.UserID); err != nil {
				return false, fmt.Errorf("failed to get user id=%s: %w", ride.UserID, err)
			}
			usersMinimalCache.Store(ride.UserID, user)
		}

		if err := writeSSE(w, &chairGetNotificationResponseData{
			RideID: ride.ID,
			User: simpleUser{
				ID:   user.ID,
				Name: fmt.Sprintf("%s %s", user.Firstname, user.Lastname),
			},
			PickupCoordinate: Coordinate{
				Latitude:  ride.PickupLatitude,
				Longitude: ride.PickupLongitude,
			},
			DestinationCoordinate: Coordinate{
				Latitude:  ride.DestinationLatitude,
				Longitude: ride.DestinationLongitude,
			},
			Status: status,
		}); err != nil {
			return false, fmt.Errorf("failed to writeSSE: %w", err)
		}
		lastRide = ride
		lastRideStatus = status
		if status == "COMPLETED" {
			chairsInRide.Delete(chair.ID)
		}
		return true, nil
	}

	// 初回送信を必ず行う
	if err := writeSSE(w, nil); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to send sse at first: %w", err))
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return

		default:
			_, err := f()
			if err != nil {
				slog.Warn("failed to send sse:", "error", err)
				return
			}
		}
	}
}

func writeSSE(w http.ResponseWriter, data interface{}) error {
	buf, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = w.Write([]byte("data: " + string(buf) + "\n\n"))
	if err != nil {
		return err
	}

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	return nil
}
