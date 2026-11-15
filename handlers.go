package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type itemRecord struct {
	ItemID   int64  `json:"id"`
	ItemName string `json:"name"`
	ItemNote string `json:"note"`
	// TagID comes from migration 0004_items_tag_id, the migration that is
	// deliberately broken on this branch.
	TagID     *int64    `json:"tag_id"`
	CreatedAt time.Time `json:"created_at"`
}

type itemRequest struct {
	ItemName string `json:"name"`
	ItemNote string `json:"note"`
	TagID    *int64 `json:"tag_id"`
}

type healthResponse struct {
	Status  string `json:"status"`
	Schema  int    `json:"schema"`
	Release string `json:"release"`
}

type application struct {
	database *pgxpool.Pool
}

func newRouter(service *application) http.Handler {
	router := http.NewServeMux()
	router.HandleFunc("GET /health", service.handleHealth)
	router.HandleFunc("GET /api/items", service.handleListItems)
	router.HandleFunc("POST /api/items", service.handleCreateItem)
	router.HandleFunc("GET /api/items/{itemID}", service.handleGetItem)
	router.HandleFunc("PUT /api/items/{itemID}", service.handleUpdateItem)
	router.HandleFunc("DELETE /api/items/{itemID}", service.handleDeleteItem)
	router.HandleFunc("GET /api/tags", service.handleListTags)
	router.HandleFunc("POST /api/tags", service.handleCreateTag)
	return withCORS(router)
}

func withCORS(nextHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(responseWriter http.ResponseWriter, httpRequest *http.Request) {
		responseWriter.Header().Set("Access-Control-Allow-Origin", "*")
		responseWriter.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		responseWriter.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if httpRequest.Method == http.MethodOptions {
			responseWriter.WriteHeader(http.StatusNoContent)
			return
		}
		nextHandler.ServeHTTP(responseWriter, httpRequest)
	})
}

func (service *application) handleHealth(responseWriter http.ResponseWriter, request *http.Request) {
	schemaVersion, schemaError := readSchemaVersion(request.Context(), service.database)
	if schemaError != nil {
		writeJSON(responseWriter, http.StatusServiceUnavailable, healthResponse{Status: "unavailable"})
		return
	}
	writeJSON(responseWriter, http.StatusOK, healthResponse{
		Status: "ok", Schema: schemaVersion, Release: releaseTag,
	})
}

const itemColumns = `id, name, note, tag_id, created_at`

func scanItem(row pgx.Row) (itemRecord, error) {
	var record itemRecord
	scanError := row.Scan(&record.ItemID, &record.ItemName, &record.ItemNote, &record.TagID, &record.CreatedAt)
	return record, scanError
}

func (service *application) handleListItems(responseWriter http.ResponseWriter, request *http.Request) {
	rows, queryError := service.database.Query(
		request.Context(), `SELECT `+itemColumns+` FROM items ORDER BY id`,
	)
	if queryError != nil {
		writeError(responseWriter, http.StatusInternalServerError, queryError.Error())
		return
	}
	defer rows.Close()
	items := []itemRecord{}
	for rows.Next() {
		record, scanError := scanItem(rows)
		if scanError != nil {
			writeError(responseWriter, http.StatusInternalServerError, scanError.Error())
			return
		}
		items = append(items, record)
	}
	if rowsError := rows.Err(); rowsError != nil {
		writeError(responseWriter, http.StatusInternalServerError, rowsError.Error())
		return
	}
	writeJSON(responseWriter, http.StatusOK, items)
}

func (service *application) handleCreateItem(responseWriter http.ResponseWriter, request *http.Request) {
	var payload itemRequest
	if decodeError := json.NewDecoder(request.Body).Decode(&payload); decodeError != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid JSON body")
		return
	}
	itemName := strings.TrimSpace(payload.ItemName)
	if itemName == "" {
		writeError(responseWriter, http.StatusBadRequest, "name is required")
		return
	}
	record, scanError := scanItem(service.database.QueryRow(
		request.Context(),
		`INSERT INTO items (name, note, tag_id) VALUES ($1, $2, $3) RETURNING `+itemColumns,
		itemName, payload.ItemNote, payload.TagID,
	))
	if scanError != nil {
		writeError(responseWriter, http.StatusInternalServerError, scanError.Error())
		return
	}
	writeJSON(responseWriter, http.StatusCreated, record)
}

func parseItemID(responseWriter http.ResponseWriter, request *http.Request) (int64, bool) {
	itemID, parseError := strconv.ParseInt(request.PathValue("itemID"), 10, 64)
	if parseError != nil || itemID <= 0 {
		writeError(responseWriter, http.StatusBadRequest, "item id must be a positive integer")
		return 0, false
	}
	return itemID, true
}

func (service *application) handleGetItem(responseWriter http.ResponseWriter, request *http.Request) {
	itemID, valid := parseItemID(responseWriter, request)
	if !valid {
		return
	}
	record, scanError := scanItem(service.database.QueryRow(
		request.Context(), `SELECT `+itemColumns+` FROM items WHERE id = $1`, itemID,
	))
	if errors.Is(scanError, pgx.ErrNoRows) {
		writeError(responseWriter, http.StatusNotFound, "item not found")
		return
	}
	if scanError != nil {
		writeError(responseWriter, http.StatusInternalServerError, scanError.Error())
		return
	}
	writeJSON(responseWriter, http.StatusOK, record)
}

func (service *application) handleUpdateItem(responseWriter http.ResponseWriter, request *http.Request) {
	itemID, valid := parseItemID(responseWriter, request)
	if !valid {
		return
	}
	var payload itemRequest
	if decodeError := json.NewDecoder(request.Body).Decode(&payload); decodeError != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid JSON body")
		return
	}
	itemName := strings.TrimSpace(payload.ItemName)
	if itemName == "" {
		writeError(responseWriter, http.StatusBadRequest, "name is required")
		return
	}
	record, scanError := scanItem(service.database.QueryRow(
		request.Context(),
		`UPDATE items SET name = $2, note = $3, tag_id = $4 WHERE id = $1 RETURNING `+itemColumns,
		itemID, itemName, payload.ItemNote, payload.TagID,
	))
	if errors.Is(scanError, pgx.ErrNoRows) {
		writeError(responseWriter, http.StatusNotFound, "item not found")
		return
	}
	if scanError != nil {
		writeError(responseWriter, http.StatusInternalServerError, scanError.Error())
		return
	}
	writeJSON(responseWriter, http.StatusOK, record)
}

func (service *application) handleDeleteItem(responseWriter http.ResponseWriter, request *http.Request) {
	itemID, valid := parseItemID(responseWriter, request)
	if !valid {
		return
	}
	result, deleteError := service.database.Exec(request.Context(), `DELETE FROM items WHERE id = $1`, itemID)
	if deleteError != nil {
		writeError(responseWriter, http.StatusInternalServerError, deleteError.Error())
		return
	}
	if result.RowsAffected() == 0 {
		writeError(responseWriter, http.StatusNotFound, "item not found")
		return
	}
	responseWriter.WriteHeader(http.StatusNoContent)
}

func writeJSON(responseWriter http.ResponseWriter, statusCode int, payload any) {
	responseWriter.Header().Set("Content-Type", "application/json")
	responseWriter.WriteHeader(statusCode)
	encoder := json.NewEncoder(responseWriter)
	encoder.SetEscapeHTML(false)
	if encodeError := encoder.Encode(payload); encodeError != nil {
		log.Printf("encode response: %v", encodeError)
	}
}

func writeError(responseWriter http.ResponseWriter, statusCode int, message string) {
	writeJSON(responseWriter, statusCode, map[string]string{"error": message})
}
