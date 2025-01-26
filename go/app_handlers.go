package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/oklog/ulid/v2"
)

var rideCache = sync.Map{}

type appPostUsersRequest struct {
	Username       string  `json:"username"`
	FirstName      string  `json:"firstname"`
	LastName       string  `json:"lastname"`
	DateOfBirth    string  `json:"date_of_birth"`
	InvitationCode *string `json:"invitation_code"`
}

type appPostUsersResponse struct {
	ID             string `json:"id"`
	InvitationCode string `json:"invitation_code"`
}

func appPostUsers(w http.ResponseWriter, r *http.Request) {
	req := &appPostUsersRequest{}
	if err := bindJSON(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Username == "" || req.FirstName == "" || req.LastName == "" || req.DateOfBirth == "" {
		writeError(w, http.StatusBadRequest, errors.New("required fields(username, firstname, lastname, date_of_birth) are empty"))
		return
	}

	userID := ulid.Make().String()
	accessToken := secureRandomStr(32)
	invitationCode := secureRandomStr(15)

	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	tx2, err := db2.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx2.Rollback()

	_, err = tx.Exec(
		"INSERT INTO users (id, username, firstname, lastname, date_of_birth, access_token, invitation_code) VALUES (?, ?, ?, ?, ?, ?, ?)",
		userID, req.Username, req.FirstName, req.LastName, req.DateOfBirth, accessToken, invitationCode,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// 初回登録キャンペーンのクーポンを付与
	_, err = tx2.Exec(
		"INSERT INTO coupons (user_id, code, discount) VALUES (?, ?, ?)",
		userID, "CP_NEW2024", 3000,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// 招待コードを使った登録
	if req.InvitationCode != nil && *req.InvitationCode != "" {
		// 招待する側の招待数をチェック
		var coupons []Coupon
		err = tx2.Select(&coupons, "SELECT * FROM coupons WHERE code = ? AND user_id = ? FOR UPDATE", "INV_"+*req.InvitationCode, userID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if len(coupons) >= 3 {
			writeError(w, http.StatusBadRequest, errors.New("この招待コードは使用できません。"))
			return
		}

		// ユーザーチェック
		var inviter User
		err = tx.Get(&inviter, "SELECT * FROM users WHERE invitation_code = ?", *req.InvitationCode)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusBadRequest, errors.New("この招待コードは使用できません。"))
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		// 招待クーポン付与
		_, err = tx2.Exec(
			"INSERT INTO coupons (user_id, code, discount) VALUES (?, ?, ?)",
			userID, "INV_"+*req.InvitationCode, 1500,
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		// 招待した人にもRewardを付与
		_, err = tx2.Exec(
			"INSERT INTO coupons (user_id, code, discount) VALUES (?, CONCAT(?, '_', FLOOR(UNIX_TIMESTAMP(NOW(3))*1000)), ?)",
			inviter.ID, "RWD_"+*req.InvitationCode, 1000,
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx2.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Path:  "/",
		Name:  "app_session",
		Value: accessToken,
	})

	writeJSON(w, http.StatusCreated, &appPostUsersResponse{
		ID:             userID,
		InvitationCode: invitationCode,
	})
}

type appPostPaymentMethodsRequest struct {
	Token string `json:"token"`
}

func appPostPaymentMethods(w http.ResponseWriter, r *http.Request) {
	req := &appPostPaymentMethodsRequest{}
	if err := bindJSON(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, errors.New("token is required but was empty"))
		return
	}

	user := r.Context().Value("user").(*User)

	_, err := db.Exec(
		`INSERT INTO payment_tokens (user_id, token) VALUES (?, ?)`,
		user.ID,
		req.Token,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type getAppRidesResponse struct {
	Rides []getAppRidesResponseItem `json:"rides"`
}

type getAppRidesResponseItem struct {
	ID                    string                       `json:"id"`
	PickupCoordinate      Coordinate                   `json:"pickup_coordinate"`
	DestinationCoordinate Coordinate                   `json:"destination_coordinate"`
	Chair                 getAppRidesResponseItemChair `json:"chair"`
	Fare                  int                          `json:"fare"`
	Evaluation            int                          `json:"evaluation"`
	RequestedAt           int64                        `json:"requested_at"`
	CompletedAt           int64                        `json:"completed_at"`
}

type getAppRidesResponseItemChair struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	Name  string `json:"name"`
	Model string `json:"model"`
}

func appGetRides(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)

	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	tx2, err := db2.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx2.Rollback()

	rides := []Ride{}
	if err := tx.Select(
		&rides,
		`SELECT * FROM rides WHERE user_id = ? AND evaluation IS NOT NULL ORDER BY id DESC`,
		user.ID,
	); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	items := []getAppRidesResponseItem{}
	for _, ride := range rides {
		fare, err := calculateDiscountedFare(tx2, user.ID, &ride, ride.PickupLatitude, ride.PickupLongitude, ride.DestinationLatitude, ride.DestinationLongitude)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		item := getAppRidesResponseItem{
			ID:                    ride.ID,
			PickupCoordinate:      Coordinate{Latitude: ride.PickupLatitude, Longitude: ride.PickupLongitude},
			DestinationCoordinate: Coordinate{Latitude: ride.DestinationLatitude, Longitude: ride.DestinationLongitude},
			Fare:                  fare,
			Evaluation:            *ride.Evaluation,
			RequestedAt:           ride.CreatedAt.UnixMilli(),
			CompletedAt:           ride.UpdatedAt.UnixMilli(),
		}

		item.Chair = getAppRidesResponseItemChair{}

		chair := &Chair{}
		if v, ok := chairMinimalCache.Load(ride.ChairID); ok {
			chair = v.(*Chair)
		} else {
			if err := tx.Get(chair, `SELECT * FROM chairs WHERE id = ?`, ride.ChairID); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			chairMinimalCache.Store(ride.ChairID, chair)
		}
		item.Chair.ID = chair.ID
		item.Chair.Name = chair.Name
		item.Chair.Model = chair.Model

		owner := &Owner{}
		if err := db2.Get(owner, `SELECT * FROM owners WHERE id = ?`, chair.OwnerID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		item.Chair.Owner = owner.Name

		items = append(items, item)
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, &getAppRidesResponse{
		Rides: items,
	})
}

type appPostRidesRequest struct {
	PickupCoordinate      *Coordinate `json:"pickup_coordinate"`
	DestinationCoordinate *Coordinate `json:"destination_coordinate"`
}

type appPostRidesResponse struct {
	RideID string `json:"ride_id"`
	Fare   int    `json:"fare"`
}

type executableGet interface {
	Get(dest interface{}, query string, args ...interface{}) error
}

// var rideStatusCache = sync.Map{}

type rideStatus struct {
	RideID    string
	Status    string
	UpdatedAt time.Time
}

func getLatestRideStatus(tx executableGet, rideID string) (string, error) {
	var s string
	if err := tx.Get(&s, `SELECT status FROM ride_status WHERE ride_id = ?`, rideID); err != nil {
		return "", err
	}
	return s, nil
}

func appPostRides(w http.ResponseWriter, r *http.Request) {
	req := &appPostRidesRequest{}
	if err := bindJSON(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.PickupCoordinate == nil || req.DestinationCoordinate == nil {
		writeError(w, http.StatusBadRequest, errors.New("required fields(pickup_coordinate, destination_coordinate) are empty"))
		return
	}

	user := r.Context().Value("user").(*User)
	rideID := ulid.Make().String()

	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	tx2, err := db2.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx2.Rollback()

	var continuingRideCount int
	if err := tx.Get(&continuingRideCount, `SELECT count(*) FROM rides WHERE user_id = ? AND evaluation IS NULL`, user.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if continuingRideCount > 0 {
		writeError(w, http.StatusConflict, errors.New("ride already exists"))
		return
	}

	if _, err := tx.Exec(
		`INSERT INTO rides (id, user_id, pickup_latitude, pickup_longitude, destination_latitude, destination_longitude)
				  VALUES (?, ?, ?, ?, ?, ?)`,
		rideID, user.ID, req.PickupCoordinate.Latitude, req.PickupCoordinate.Longitude, req.DestinationCoordinate.Latitude, req.DestinationCoordinate.Longitude,
	); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// rideStatusCache.Store(rideID, rideStatus{rideID, "MATCHING", time.Now()})
	if _, err := tx2.Exec(`INSERT INTO ride_status (ride_id, status) VALUES (?, 'MATCHING')`, rideID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	var rideCount int
	if err := tx.Get(&rideCount, `SELECT COUNT(*) FROM rides WHERE user_id = ? `, user.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	var coupon Coupon
	if rideCount == 1 {
		// 初回利用で、初回利用クーポンがあれば必ず使う
		if err := tx2.Get(&coupon, "SELECT * FROM coupons WHERE user_id = ? AND code = 'CP_NEW2024' AND used_by IS NULL FOR UPDATE", user.ID); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusInternalServerError, err)
				return
			}

			// 無ければ他のクーポンを付与された順番に使う
			if err := tx2.Get(&coupon, "SELECT * FROM coupons WHERE user_id = ? AND used_by IS NULL ORDER BY created_at LIMIT 1 FOR UPDATE", user.ID); err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					writeError(w, http.StatusInternalServerError, err)
					return
				}
			} else {
				if _, err := tx2.Exec(
					"UPDATE coupons SET used_by = ? WHERE user_id = ? AND code = ?",
					rideID, user.ID, coupon.Code,
				); err != nil {
					writeError(w, http.StatusInternalServerError, err)
					return
				}
			}
		} else {
			if _, err := tx2.Exec(
				"UPDATE coupons SET used_by = ? WHERE user_id = ? AND code = 'CP_NEW2024'",
				rideID, user.ID,
			); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
	} else {
		// 他のクーポンを付与された順番に使う
		if err := tx2.Get(&coupon, "SELECT * FROM coupons WHERE user_id = ? AND used_by IS NULL ORDER BY created_at LIMIT 1 FOR UPDATE", user.ID); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		} else {
			if _, err := tx2.Exec(
				"UPDATE coupons SET used_by = ? WHERE user_id = ? AND code = ?",
				rideID, user.ID, coupon.Code,
			); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
	}

	ride := Ride{
		ID:                   rideID,
		UserID:               user.ID,
		PickupLatitude:       req.PickupCoordinate.Latitude,
		PickupLongitude:      req.PickupCoordinate.Longitude,
		DestinationLatitude:  req.DestinationCoordinate.Latitude,
		DestinationLongitude: req.DestinationCoordinate.Longitude,
	}

	fare, err := calculateDiscountedFare(tx2, user.ID, &ride, req.PickupCoordinate.Latitude, req.PickupCoordinate.Longitude, req.DestinationCoordinate.Latitude, req.DestinationCoordinate.Longitude)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx2.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusAccepted, &appPostRidesResponse{
		RideID: rideID,
		Fare:   fare,
	})
}

type appPostRidesEstimatedFareRequest struct {
	PickupCoordinate      *Coordinate `json:"pickup_coordinate"`
	DestinationCoordinate *Coordinate `json:"destination_coordinate"`
}

type appPostRidesEstimatedFareResponse struct {
	Fare     int `json:"fare"`
	Discount int `json:"discount"`
}

func appPostRidesEstimatedFare(w http.ResponseWriter, r *http.Request) {
	req := &appPostRidesEstimatedFareRequest{}
	if err := bindJSON(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.PickupCoordinate == nil || req.DestinationCoordinate == nil {
		writeError(w, http.StatusBadRequest, errors.New("required fields(pickup_coordinate, destination_coordinate) are empty"))
		return
	}

	user := r.Context().Value("user").(*User)

	tx2, err := db2.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx2.Rollback()

	discounted, err := calculateDiscountedFare(tx2, user.ID, nil, req.PickupCoordinate.Latitude, req.PickupCoordinate.Longitude, req.DestinationCoordinate.Latitude, req.DestinationCoordinate.Longitude)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if err := tx2.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, &appPostRidesEstimatedFareResponse{
		Fare:     discounted,
		Discount: calculateFare(req.PickupCoordinate.Latitude, req.PickupCoordinate.Longitude, req.DestinationCoordinate.Latitude, req.DestinationCoordinate.Longitude) - discounted,
	})
}

// マンハッタン距離を求める
func calculateDistance(aLatitude, aLongitude, bLatitude, bLongitude int) int {
	return abs(aLatitude-bLatitude) + abs(aLongitude-bLongitude)
}
func abs(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

type appPostRideEvaluationRequest struct {
	Evaluation int `json:"evaluation"`
}

type appPostRideEvaluationResponse struct {
	CompletedAt int64 `json:"completed_at"`
}

var urlCache = sync.Map{}

func appPostRideEvaluatation(w http.ResponseWriter, r *http.Request) {
	rideID := r.PathValue("ride_id")

	req := &appPostRideEvaluationRequest{}
	if err := bindJSON(r, req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Evaluation < 1 || req.Evaluation > 5 {
		writeError(w, http.StatusBadRequest, errors.New("evaluation must be between 1 and 5"))
		return
	}

	tx, err := db.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx.Rollback()
	tx2, err := db2.Beginx()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer tx2.Rollback()

	var ride *Ride
	if v, ok := rideCache.Load(rideID); ok {
		ride = v.(*Ride)
	} else {
		ride = &Ride{}
		if err := tx.Get(ride, `SELECT * FROM rides WHERE id = ?`, rideID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, errors.New("ride not found"))
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		rideCache.Store(rideID, ride)
	}
	status, err := getLatestRideStatus(tx2, ride.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if status == "COMPLETED" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("already completed status=%s ride_id=%s", status, rideID))
		return
	}
	if status != "ARRIVED" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("not arrived yet status=%s ride_id=%s", status, rideID))
		return
	}

	rideCache.Delete(rideID)
	result, err := tx.Exec(
		`UPDATE rides SET evaluation = ? WHERE id = ?`,
		req.Evaluation, rideID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if count, err := result.RowsAffected(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	} else if count == 0 {
		writeError(w, http.StatusNotFound, errors.New("ride not found"))
		return
	}

	// rideStatusCache.Store(rideID, rideStatus{rideID, "COMPLETED", time.Now()})
	if _, err := tx2.Exec(`UPDATE ride_status SET status = 'COMPLETED' WHERE ride_id = ?`, rideID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if v, ok := rideCache.Load(rideID); ok {
		ride = v.(*Ride)
	} else {
		if err := tx.Get(ride, `SELECT * FROM rides WHERE id = ?`, rideID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, errors.New("ride not found"))
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		rideCache.Store(rideID, ride)
	}

	paymentToken := &PaymentToken{}
	if err := tx.Get(paymentToken, `SELECT * FROM payment_tokens WHERE user_id = ?`, ride.UserID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusBadRequest, errors.New("payment token not registered"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	fare, err := calculateDiscountedFare(tx2, ride.UserID, ride, ride.PickupLatitude, ride.PickupLongitude, ride.DestinationLatitude, ride.DestinationLongitude)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	paymentGatewayRequest := &paymentGatewayPostPaymentRequest{
		Amount: fare,
	}

	var paymentGatewayURL string
	if u, ok := urlCache.Load("payment_gateway_url"); ok {
		paymentGatewayURL = u.(string)
	} else {
		if err := tx.Get(&paymentGatewayURL, "SELECT value FROM settings WHERE name = 'payment_gateway_url'"); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		urlCache.Store("payment_gateway_url", paymentGatewayURL)
	}

	if err := requestPaymentGatewayPostPayment(paymentGatewayURL, paymentToken.Token, paymentGatewayRequest, func() ([]Ride, error) {
		rides := []Ride{}
		if err := tx.Select(&rides, `SELECT * FROM rides WHERE user_id = ? ORDER BY id ASC`, ride.UserID); err != nil {
			return nil, err
		}
		return rides, nil
	}); err != nil {
		if errors.Is(err, erroredUpstream) {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := tx2.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	chairsInRide.Delete(ride.ChairID.String)
	slog.Debug("ride completed", "ride_id", rideID)
	sendNotificationSSE(ride.ChairID.String, ride, "COMPLETED")
	sendNotificationSSEApp(ride.UserID, ride, "COMPLETED")

	writeJSON(w, http.StatusOK, &appPostRideEvaluationResponse{
		CompletedAt: ride.UpdatedAt.UnixMilli(),
	})
}

type appGetNotificationResponse struct {
	Data *appGetNotificationResponseData `json:"data"`
}

type appGetNotificationResponseData struct {
	RideID                string                           `json:"ride_id"`
	PickupCoordinate      Coordinate                       `json:"pickup_coordinate"`
	DestinationCoordinate Coordinate                       `json:"destination_coordinate"`
	Fare                  int                              `json:"fare"`
	Status                string                           `json:"status"`
	Chair                 *appGetNotificationResponseChair `json:"chair,omitempty"`
	CreatedAt             int64                            `json:"created_at"`
	UpdateAt              int64                            `json:"updated_at"`
}

type appGetNotificationResponseChair struct {
	ID    string                               `json:"id"`
	Name  string                               `json:"name"`
	Model string                               `json:"model"`
	Stats appGetNotificationResponseChairStats `json:"stats"`
}

type appGetNotificationResponseChairStats struct {
	TotalRidesCount    int     `json:"total_rides_count"`
	TotalEvaluationAvg float64 `json:"total_evaluation_avg"`
}

func getChairStats(tx *sqlx.Tx, tx2 *sqlx.Tx, chairID string) (appGetNotificationResponseChairStats, error) {
	stats := appGetNotificationResponseChairStats{}

	var result struct {
		TotalRidesCount    int     `db:"c"`
		TotalEvaluationAvg float64 `db:"s"`
	}
	err := tx.Get(
		&result,
		`SELECT count(*) as c, ifnull(sum(evaluation),0) as s FROM rides WHERE chair_id = ? AND evaluation IS NOT NULL`,
		chairID,
	)
	if err != nil {
		return stats, err
	}

	totalRideCount := result.TotalRidesCount
	totalEvaluation := result.TotalEvaluationAvg

	stats.TotalRidesCount = totalRideCount
	if totalRideCount > 0 {
		stats.TotalEvaluationAvg = totalEvaluation / float64(totalRideCount)
	}

	return stats, nil
}

type appGetNearbyChairsResponse struct {
	Chairs      []appGetNearbyChairsResponseChair `json:"chairs"`
	RetrievedAt int64                             `json:"retrieved_at"`
}

type appGetNearbyChairsResponseChair struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Model             string     `json:"model"`
	CurrentCoordinate Coordinate `json:"current_coordinate"`
}

func appGetNearbyChairs(w http.ResponseWriter, r *http.Request) {
	latStr := r.URL.Query().Get("latitude")
	lonStr := r.URL.Query().Get("longitude")
	distanceStr := r.URL.Query().Get("distance")
	if latStr == "" || lonStr == "" {
		writeError(w, http.StatusBadRequest, errors.New("latitude or longitude is empty"))
		return
	}

	lat, err := strconv.Atoi(latStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("latitude is invalid"))
		return
	}

	lon, err := strconv.Atoi(lonStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("longitude is invalid"))
		return
	}

	distance := 50
	if distanceStr != "" {
		distance, err = strconv.Atoi(distanceStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("distance is invalid"))
			return
		}
	}

	/*
		var chairs []*Chair
		if v, ok := nearByChairsCache.Load("now"); ok {
			chairs = v.([]*Chair)
		} else {
			writeError(w, http.StatusInternalServerError, errors.New("chairs cache is empty"))
			return
		}
	*/
	chairs := make([]*Chair, 0, 1000)
	if err := db.Select(
		&chairs,
		`SELECT chairs.id, name, model, latitude, longitude FROM chairs
				WHERE is_active = 1 AND latitude IS NOT NULL
				AND ABS(latitude - ?) + ABS(longitude - ?) <= ?`,
		lat, lon, distance,
	); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	nearbyChairs := []appGetNearbyChairsResponseChair{}
	for _, chair := range chairs {
		if _, ok := chairsInRide.Load(chair.ID); ok {
			continue
		}
		if chair.Latitude == nil || chair.Longitude == nil {
			continue
		}
		//		if calculateDistance(lat, lon, *chair.Latitude, *chair.Longitude) > distance {
		//			continue
		//		}
		nearbyChairs = append(nearbyChairs, appGetNearbyChairsResponseChair{
			ID:    chair.ID,
			Name:  chair.Name,
			Model: chair.Model,
			CurrentCoordinate: Coordinate{
				Latitude:  *chair.Latitude,
				Longitude: *chair.Longitude,
			},
		})
	}

	retrievedAt := time.Now()
	writeJSON(w, http.StatusOK, &appGetNearbyChairsResponse{
		Chairs:      nearbyChairs,
		RetrievedAt: retrievedAt.UnixMilli(),
	})
}

func calculateFare(pickupLatitude, pickupLongitude, destLatitude, destLongitude int) int {
	meteredFare := farePerDistance * calculateDistance(pickupLatitude, pickupLongitude, destLatitude, destLongitude)
	return initialFare + meteredFare
}

func calculateDiscountedFare(tx *sqlx.Tx, userID string, ride *Ride, pickupLatitude, pickupLongitude, destLatitude, destLongitude int) (int, error) {
	var coupon Coupon
	discount := 0
	if ride != nil {
		destLatitude = ride.DestinationLatitude
		destLongitude = ride.DestinationLongitude
		pickupLatitude = ride.PickupLatitude
		pickupLongitude = ride.PickupLongitude

		// すでにクーポンが紐づいているならそれの割引額を参照
		if err := tx.Get(&coupon, "SELECT * FROM coupons WHERE used_by = ?", ride.ID); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return 0, err
			}
		} else {
			discount = coupon.Discount
		}
	} else {
		// 初回利用クーポンを最優先で使う
		if err := tx.Get(&coupon, "SELECT * FROM coupons WHERE user_id = ? AND code = 'CP_NEW2024' AND used_by IS NULL", userID); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return 0, err
			}

			// 無いなら他のクーポンを付与された順番に使う
			if err := tx.Get(&coupon, "SELECT * FROM coupons WHERE user_id = ? AND used_by IS NULL ORDER BY created_at LIMIT 1", userID); err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					return 0, err
				}
			} else {
				discount = coupon.Discount
			}
		} else {
			discount = coupon.Discount
		}
	}

	meteredFare := farePerDistance * calculateDistance(pickupLatitude, pickupLongitude, destLatitude, destLongitude)
	discountedMeteredFare := max(meteredFare-discount, 0)

	return initialFare + discountedMeteredFare, nil
}

var appChannels = sync.Map{}

func sendNotificationSSEApp(userID string, ride *Ride, status string) {
	if userID == "" {
		panic("userID is empty")
	}
	if ride == nil {
		panic("ride is nil")
	}
	if status == "" {
		panic("status is empty")
	}
	if !ride.ChairID.Valid {
		slog.Warn("chairID is invalid", "ride", *ride, "status", status)
		return
	}
	_ch, _ := appChannels.LoadOrStore(userID, make(chan notify, chanSize))
	ch := _ch.(chan notify)
	select {
	case ch <- notify{Ride: ride, Status: status}:
	default:
		log.Println("dropped notification", userID, ride.ID, status)
		// non-blocking
	}
}

var chairMinimalCache = sync.Map{}

func appGetNotificationSSE(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)

	_ch, _ := appChannels.LoadOrStore(user.ID, make(chan notify, chanSize))
	ch := _ch.(chan notify)

	// Server Sent Events
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	var lastRide *Ride
	var lastRideStatus string
	f := func() (respond bool, err error) {
		slog.Debug("waiting", "user", user.ID)
		n := <-ch
		slog.Debug("received", "user notification", n)
		ride := n.Ride
		status := n.Status

		if lastRide != nil && ride.ID == lastRide.ID && status == lastRideStatus {
			return false, nil
		}

		tx, err := db.Beginx()
		if err != nil {
			return false, err
		}
		defer tx.Rollback()
		tx2, err := db2.Beginx()
		if err != nil {
			return false, err
		}
		defer tx2.Rollback()

		fare, err := calculateDiscountedFare(tx2, user.ID, ride, ride.PickupLatitude, ride.PickupLongitude, ride.DestinationLatitude, ride.DestinationLongitude)
		if err != nil {
			return false, err
		}

		chair := &Chair{}
		stats := appGetNotificationResponseChairStats{}
		if ride.ChairID.Valid {
			if v, ok := chairMinimalCache.Load(ride.ChairID.String); ok {
				chair = v.(*Chair)
			} else {
				if err := tx.Get(chair, `SELECT * FROM chairs WHERE id = ?`, ride.ChairID); err != nil {
					return false, err
				}
				chairMinimalCache.Store(ride.ChairID.String, chair)
			}
			stats, err = getChairStats(tx, tx2, ride.ChairID.String)
			if err != nil {
				return false, err
			}
		}

		if err := writeSSE(w, &appGetNotificationResponseData{
			RideID: ride.ID,
			PickupCoordinate: Coordinate{
				Latitude:  ride.PickupLatitude,
				Longitude: ride.PickupLongitude,
			},
			DestinationCoordinate: Coordinate{
				Latitude:  ride.DestinationLatitude,
				Longitude: ride.DestinationLongitude,
			},
			Fare:   fare,
			Status: status,
			Chair: &appGetNotificationResponseChair{
				ID:    chair.ID,
				Name:  chair.Name,
				Model: chair.Model,
				Stats: stats,
			},
			CreatedAt: ride.CreatedAt.UnixMilli(),
			UpdateAt:  ride.UpdatedAt.UnixMilli(),
		}); err != nil {
			return false, err
		}
		lastRide = ride
		lastRideStatus = status

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
