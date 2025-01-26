package main

import (
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/samber/lo"
)

type matching struct {
	Ride  *Ride
	Chair *Chair
	Score float64
	PD    int
	DD    int
	Age   float64
	Speed int
}

var chairsInRide = sync.Map{}
var nearByChairsCache = sync.Map{}

var ticker = time.NewTicker(100 * time.Millisecond)

type nearByChair struct {
	Chair
	RideID sql.NullString `db:"ride_id"`
}

func internalChairsCache(w http.ResponseWriter, _ *http.Request) {
	<-ticker.C
	chairs := make([]*Chair, 0, 1000)
	err := db.Select(
		&chairs,
		`SELECT chairs.id, name, model, latitude, longitude FROM chairs WHERE is_active = 1`,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	nearByChairsCache.Store("now", chairs)
	slog.Info("cached", "chairs", len(chairs))
	w.WriteHeader(http.StatusNoContent)
}

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる
func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	rides := []*Ride{}
	if err := db.Select(&rides, `SELECT * FROM rides WHERE chair_id IS NULL ORDER BY id`); err != nil {
		if errors.Is(err, sql.ErrNoRows) || len(rides) == 0 {
			slog.Info("no rides for waiting", "err", err)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	chairs := make([]struct {
		Chair
		Speed int `db:"speed"`
	}, 0, 1000)
	if err := db.Select(&chairs,
		`SELECT *, speed FROM chairs JOIN chair_models ON (chairs.model=chair_models.name) WHERE is_active = TRUE AND latitude IS NOT NULL`,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) || len(chairs) == 0 {
			slog.Info("no active chairs", "err", err)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	//chairとrideのマッチングをするためにスコアを計算
	matchings := []matching{}
	freeChairsCount := 0
	for _, chair := range chairs {
		if _, ok := chairsInRide.Load(chair.ID); ok {
			// ride中の椅子はスキップ
			continue
		}
		freeChairsCount++
		for _, ride := range rides {
			pickupDistance := calculateDistance(*chair.Latitude, *chair.Longitude, ride.PickupLatitude, ride.PickupLongitude)
			destinationDistance := calculateDistance(ride.PickupLatitude, ride.PickupLongitude, ride.DestinationLatitude, ride.DestinationLongitude)
			age := time.Since(ride.CreatedAt).Seconds()
			var score float64
			// pickupDistanceは少ないほどよい
			if pickupDistance == 0 {
				score += 250
			} else {
				score += 250 / float64(pickupDistance)
			}
			//destinationDistanceは多いほどよい
			// score += float64(destinationDistance) / 10

			// ageが古いやつから優先
			score += 10 * age

			if age > 20 {
				score += 10000 // 最優先
			}

			matchings = append(matchings, matching{
				Ride: ride, Chair: &chair.Chair, Score: score,
				PD: pickupDistance, DD: destinationDistance, Age: age,
				Speed: chair.Speed,
			})
		}
	}
	slog.Info("count", "chairs", len(chairs), "free", freeChairsCount, "rides", len(rides))

	// スコアが高い順に並び替え
	sort.SliceStable(matchings, func(i, j int) bool {
		return matchings[i].Score > matchings[j].Score
	})
	matchedRides := map[string]bool{}
	matchedChairs := map[string]bool{}
	comletedMatchings := []matching{}

	for _, m := range matchings {
		if matchedRides[m.Ride.ID] || matchedChairs[m.Chair.ID] {
			continue
		}
		if m.PD > 50 && m.Score < 10000 {
			// 遠すぎる
			continue
		}
		/*
			if m.Score > 10000 && m.Speed >= 5 {
				// どうせ待たせてるので速いやつを使うのはもったいない
				continue
			}
		*/
		matchedRides[m.Ride.ID] = true
		matchedChairs[m.Chair.ID] = true
		comletedMatchings = append(comletedMatchings, m)
	}
	if len(comletedMatchings) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	matchedCount := 0
	var maxAge float64
	for _, chunk := range lo.Chunk(comletedMatchings, 40) {
		notifies := map[string]notify{}
		tx, err := db.Beginx()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		for _, m := range chunk {
			maxAge = math.Max(maxAge, m.Age)
			slog.Debug("matched", "score", m.Score, "pd", m.PD, "dd", m.DD, "age", m.Age, "speed", m.Speed)
			rideCache.Delete(m.Ride.ID)
			if _, err := tx.Exec("UPDATE rides SET chair_id = ? WHERE id = ?", m.Chair.ID, m.Ride.ID); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			notifies[m.Chair.ID] = notify{Ride: m.Ride, Status: "MATCHING"}
			m.Ride.ChairID = sql.NullString{String: m.Chair.ID, Valid: true}
			matchedCount++
		}
		tx.Commit()
		for chairID, ns := range notifies {
			sendNotificationSSE(chairID, ns.Ride, ns.Status)
			sendNotificationSSEApp(ns.Ride.UserID, ns.Ride, ns.Status)
			chairsInRide.Store(chairID, ns.Ride)
		}
		//if matchedCount >= 150 {
		//	break
		//}
	}
	avgAge := maxAge / float64(len(comletedMatchings))
	slog.Info("count", "matched", matchedCount, "remaining", len(rides)-matchedCount, "max_age", maxAge, "avg_age", avgAge)
	w.WriteHeader(http.StatusNoContent)
}
