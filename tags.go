package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// tags arrived with migration 0003_tags (schema 3) on the v3-add-tags branch.
type tagRecord struct {
	TagID     int64     `json:"id"`
	TagName   string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type tagRequest struct {
	TagName string `json:"name"`
}

const tagColumns = `id, name, created_at`

func (service *application) handleListTags(responseWriter http.ResponseWriter, request *http.Request) {
	rows, queryError := service.database.Query(
		request.Context(), `SELECT `+tagColumns+` FROM tags ORDER BY id`,
	)
	if queryError != nil {
		writeError(responseWriter, http.StatusInternalServerError, queryError.Error())
		return
	}
	defer rows.Close()
	tags := []tagRecord{}
	for rows.Next() {
		var record tagRecord
		if scanError := rows.Scan(&record.TagID, &record.TagName, &record.CreatedAt); scanError != nil {
			writeError(responseWriter, http.StatusInternalServerError, scanError.Error())
			return
		}
		tags = append(tags, record)
	}
	if rowsError := rows.Err(); rowsError != nil {
		writeError(responseWriter, http.StatusInternalServerError, rowsError.Error())
		return
	}
	writeJSON(responseWriter, http.StatusOK, tags)
}

func (service *application) handleCreateTag(responseWriter http.ResponseWriter, request *http.Request) {
	var payload tagRequest
	if decodeError := json.NewDecoder(request.Body).Decode(&payload); decodeError != nil {
		writeError(responseWriter, http.StatusBadRequest, "invalid JSON body")
		return
	}
	tagName := strings.TrimSpace(payload.TagName)
	if tagName == "" {
		writeError(responseWriter, http.StatusBadRequest, "name is required")
		return
	}
	var record tagRecord
	scanError := service.database.QueryRow(
		request.Context(),
		`INSERT INTO tags (name) VALUES ($1) RETURNING `+tagColumns,
		tagName,
	).Scan(&record.TagID, &record.TagName, &record.CreatedAt)
	var postgresError *pgconn.PgError
	if errors.As(scanError, &postgresError) && postgresError.Code == "23505" {
		writeError(responseWriter, http.StatusConflict, "tag name already exists")
		return
	}
	if scanError != nil {
		writeError(responseWriter, http.StatusInternalServerError, scanError.Error())
		return
	}
	writeJSON(responseWriter, http.StatusCreated, record)
}
