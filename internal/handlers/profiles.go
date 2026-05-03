package handlers

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/adeycodes/insighta-backend/internal/db"
	"github.com/adeycodes/insighta-backend/internal/models"
	"github.com/google/uuid"
)

// ─── Third-party API response shapes ─────────────────────────────────────────

type genderizeResponse struct {
	Gender      *string `json:"gender"`
	Probability float64 `json:"probability"`
	Count       int     `json:"count"`
}

type agifyResponse struct {
	Age *int `json:"age"`
}

type nationalizeResponse struct {
	Country []struct {
		CountryID   string  `json:"country_id"`
		Probability float64 `json:"probability"`
	} `json:"country"`
}

// ─── Country lookup ───────────────────────────────────────────────────────────

var countryNames = map[string]string{
	"NG": "Nigeria", "GH": "Ghana", "KE": "Kenya", "ZA": "South Africa",
	"TZ": "Tanzania", "UG": "Uganda", "ET": "Ethiopia", "EG": "Egypt",
	"MA": "Morocco", "DZ": "Algeria", "SN": "Senegal", "CM": "Cameroon",
	"CD": "DR Congo", "AO": "Angola", "SD": "Sudan", "BJ": "Benin",
	"US": "United States", "GB": "United Kingdom", "CA": "Canada",
	"FR": "France", "DE": "Germany", "BR": "Brazil",
	"IN": "India", "CN": "China", "JP": "Japan", "AU": "Australia",
}

func countryNameFor(id string) string {
	if name, ok := countryNames[id]; ok {
		return name
	}
	return id
}

// ─── Age classification ───────────────────────────────────────────────────────

func ageGroup(age int) string {
	switch {
	case age <= 12:
		return "child"
	case age <= 19:
		return "teenager"
	case age <= 59:
		return "adult"
	default:
		return "senior"
	}
}

// ─── External API enrichment ──────────────────────────────────────────────────

type enrichedProfile struct {
	Gender             string
	GenderProbability  float64
	SampleSize         int
	Age                int
	AgeGroup           string
	CountryID          string
	CountryName        string
	CountryProbability float64
}

// enrich calls the three external APIs concurrently and merges the results.
func enrich(name string) (*enrichedProfile, error) {
	type result[T any] struct {
		value T
		err   error
	}

	genderCh := make(chan result[genderizeResponse], 1)
	ageCh := make(chan result[agifyResponse], 1)
	countryCh := make(chan result[nationalizeResponse], 1)

	encoded := url.QueryEscape(name)

	go func() {
		var v genderizeResponse
		err := getJSON("https://api.genderize.io?name="+encoded, &v)
		genderCh <- result[genderizeResponse]{v, err}
	}()
	go func() {
		var v agifyResponse
		err := getJSON("https://api.agify.io?name="+encoded, &v)
		ageCh <- result[agifyResponse]{v, err}
	}()
	go func() {
		var v nationalizeResponse
		err := getJSON("https://api.nationalize.io?name="+encoded, &v)
		countryCh <- result[nationalizeResponse]{v, err}
	}()

	gender := <-genderCh
	age := <-ageCh
	country := <-countryCh

	if gender.err != nil {
		return nil, fmt.Errorf("genderize.io: %w", gender.err)
	}
	if age.err != nil {
		return nil, fmt.Errorf("agify.io: %w", age.err)
	}
	if country.err != nil {
		return nil, fmt.Errorf("nationalize.io: %w", country.err)
	}

	if gender.value.Gender == nil || gender.value.Count == 0 {
		return nil, fmt.Errorf("genderize.io returned no data for %q", name)
	}
	if age.value.Age == nil {
		return nil, fmt.Errorf("agify.io returned no data for %q", name)
	}
	if len(country.value.Country) == 0 {
		return nil, fmt.Errorf("nationalize.io returned no data for %q", name)
	}

	// Pick the country with the highest probability.
	top := country.value.Country[0]
	for _, c := range country.value.Country[1:] {
		if c.Probability > top.Probability {
			top = c
		}
	}

	return &enrichedProfile{
		Gender:             *gender.value.Gender,
		GenderProbability:  gender.value.Probability,
		SampleSize:         gender.value.Count,
		Age:                *age.value.Age,
		AgeGroup:           ageGroup(*age.value.Age),
		CountryID:          top.CountryID,
		CountryName:        countryNameFor(top.CountryID),
		CountryProbability: top.Probability,
	}, nil
}

var apiClient = &http.Client{Timeout: 8 * time.Second}

func getJSON(rawURL string, dst any) error {
	resp, err := apiClient.Get(rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// ─── Query building ───────────────────────────────────────────────────────────

// queryBuilder accumulates SQL conditions and positional arguments.
type queryBuilder struct {
	where    strings.Builder
	args     []any
	next     int
	sortBy   string
	sortDir  string
	page     int
	pageSize int
}

func newQueryBuilder() *queryBuilder {
	qb := &queryBuilder{next: 1, sortBy: "created_at", sortDir: "DESC", page: 1, pageSize: 10}
	qb.where.WriteString(" WHERE 1=1")
	return qb
}

func (qb *queryBuilder) addCondition(column, value string) {
	qb.where.WriteString(fmt.Sprintf(" AND LOWER(%s) = LOWER($%d)", column, qb.next))
	qb.args = append(qb.args, value)
	qb.next++
}

func (qb *queryBuilder) addIntGTE(column string, value int) {
	qb.where.WriteString(fmt.Sprintf(" AND %s >= $%d", column, qb.next))
	qb.args = append(qb.args, value)
	qb.next++
}

func (qb *queryBuilder) addIntLTE(column string, value int) {
	qb.where.WriteString(fmt.Sprintf(" AND %s <= $%d", column, qb.next))
	qb.args = append(qb.args, value)
	qb.next++
}

func (qb *queryBuilder) addFloatGTE(column string, value float64) {
	qb.where.WriteString(fmt.Sprintf(" AND %s >= $%d", column, qb.next))
	qb.args = append(qb.args, value)
	qb.next++
}

func (qb *queryBuilder) whereClause() string { return qb.where.String() }

// fromRequest parses query-string filter/sort/pagination params into the builder.
func (qb *queryBuilder) fromRequest(r *http.Request) error {
	q := r.URL.Query()

	if v := q.Get("gender"); v != "" {
		if v != "male" && v != "female" {
			return fmt.Errorf("gender must be male or female")
		}
		qb.addCondition("gender", v)
	}
	if v := q.Get("country_id"); v != "" {
		qb.addCondition("country_id", v)
	}
	if v := q.Get("age_group"); v != "" {
		valid := map[string]bool{"child": true, "teenager": true, "adult": true, "senior": true}
		if !valid[strings.ToLower(v)] {
			return fmt.Errorf("age_group must be child, teenager, adult, or senior")
		}
		qb.addCondition("age_group", v)
	}
	if v := q.Get("min_age"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("min_age must be a non-negative integer")
		}
		qb.addIntGTE("age", n)
	}
	if v := q.Get("max_age"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("max_age must be a non-negative integer")
		}
		qb.addIntLTE("age", n)
	}
	if v := q.Get("min_gender_probability"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 1 {
			return fmt.Errorf("min_gender_probability must be between 0 and 1")
		}
		qb.addFloatGTE("gender_probability", f)
	}
	if v := q.Get("min_country_probability"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 1 {
			return fmt.Errorf("min_country_probability must be between 0 and 1")
		}
		qb.addFloatGTE("country_probability", f)
	}

	// Sorting — whitelist to prevent SQL injection.
	allowed := map[string]string{
		"age": "age", "created_at": "created_at", "gender_probability": "gender_probability",
	}
	if col, ok := allowed[q.Get("sort_by")]; ok {
		qb.sortBy = col
	}
	if d := strings.ToUpper(q.Get("order")); d == "ASC" {
		qb.sortDir = "ASC"
	}

	// Pagination.
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
		qb.page = p
	}
	if s, err := strconv.Atoi(q.Get("limit")); err == nil && s > 0 {
		if s > 50 {
			s = 50
		}
		qb.pageSize = s
	}

	return nil
}

// offset returns the SQL OFFSET value for the current page.
func (qb *queryBuilder) offset() int { return (qb.page - 1) * qb.pageSize }

// listSQL returns the full SELECT statement for a paginated profile list.
func (qb *queryBuilder) listSQL() string {
	return fmt.Sprintf(
		`SELECT id, name, gender, gender_probability, sample_size, age, age_group,
		        country_id, country_name, country_probability, created_at
		 FROM profiles%s
		 ORDER BY %s %s
		 LIMIT $%d OFFSET $%d`,
		qb.whereClause(), qb.sortBy, qb.sortDir, qb.next, qb.next+1,
	)
}

// listArgs returns the arguments slice for listSQL, appending LIMIT and OFFSET.
func (qb *queryBuilder) listArgs() []any {
	return append(qb.args, qb.pageSize, qb.offset())
}

// ─── Row scanning ─────────────────────────────────────────────────────────────

func scanProfile(row *sql.Row) (*models.Profile, error) {
	var p models.Profile
	err := row.Scan(
		&p.ID, &p.Name, &p.Gender, &p.GenderProbability, &p.SampleSize,
		&p.Age, &p.AgeGroup, &p.CountryID, &p.CountryName, &p.CountryProbability, &p.CreatedAt,
	)
	return &p, err
}

func scanProfileRows(rows *sql.Rows) ([]models.Profile, error) {
	profiles := make([]models.Profile, 0)
	for rows.Next() {
		var p models.Profile
		if err := rows.Scan(
			&p.ID, &p.Name, &p.Gender, &p.GenderProbability, &p.SampleSize,
			&p.Age, &p.AgeGroup, &p.CountryID, &p.CountryName, &p.CountryProbability, &p.CreatedAt,
		); err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

// ─── Pagination response builder ──────────────────────────────────────────────

func paginatedResponse(r *http.Request, data []models.Profile, page, pageSize, total int) models.PaginatedResponse {
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}

	// Preserve all existing query params in generated links.
	buildLink := func(p int) string {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(p))
		q.Set("limit", strconv.Itoa(pageSize))
		return r.URL.Path + "?" + q.Encode()
	}

	resp := models.PaginatedResponse{
		Status:     "success",
		Page:       page,
		Limit:      pageSize,
		Total:      total,
		TotalPages: totalPages,
		Links:      models.PagingLinks{Self: buildLink(page)},
		Data:       data,
	}
	if page < totalPages {
		next := buildLink(page + 1)
		resp.Links.Next = &next
	}
	if page > 1 {
		prev := buildLink(page - 1)
		resp.Links.Prev = &prev
	}
	return resp
}

// ─── GET /api/profiles ────────────────────────────────────────────────────────

func HandleListProfiles(w http.ResponseWriter, r *http.Request) {
	database, err := db.Connect()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "service unavailable")
		return
	}

	qb := newQueryBuilder()
	if err := qb.fromRequest(r); err != nil {
		apiError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	var total int
	if err := database.QueryRow(`SELECT COUNT(*) FROM profiles`+qb.whereClause(), qb.args...).Scan(&total); err != nil {
		apiError(w, http.StatusInternalServerError, "count query failed")
		return
	}

	rows, err := database.Query(qb.listSQL(), qb.listArgs()...)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list query failed")
		return
	}
	defer rows.Close()

	profiles, err := scanProfileRows(rows)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to read profiles")
		return
	}

	respond(w, http.StatusOK, paginatedResponse(r, profiles, qb.page, qb.pageSize, total))
}

// ─── GET /api/profiles/search ─────────────────────────────────────────────────

func HandleSearchProfiles(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		apiError(w, http.StatusBadRequest, "query parameter 'q' is required")
		return
	}

	qb, err := parseNaturalLanguage(q)
	if err != nil {
		apiError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// Also apply standard pagination from query string.
	qb.fromRequest(r) //nolint:errcheck — pagination-only params can't fail

	database, err := db.Connect()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "service unavailable")
		return
	}

	var total int
	database.QueryRow(`SELECT COUNT(*) FROM profiles`+qb.whereClause(), qb.args...).Scan(&total)

	rows, err := database.Query(qb.listSQL(), qb.listArgs()...)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "search query failed")
		return
	}
	defer rows.Close()

	profiles, err := scanProfileRows(rows)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to read profiles")
		return
	}

	respond(w, http.StatusOK, paginatedResponse(r, profiles, qb.page, qb.pageSize, total))
}

// ─── GET /api/profiles/export ─────────────────────────────────────────────────

func HandleExportCSV(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("format") != "csv" {
		apiError(w, http.StatusBadRequest, "format=csv is required")
		return
	}

	database, err := db.Connect()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "service unavailable")
		return
	}

	qb := newQueryBuilder()
	if err := qb.fromRequest(r); err != nil {
		apiError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// Export all matching rows — no pagination limit.
	exportSQL := fmt.Sprintf(
		`SELECT id, name, gender, gender_probability, sample_size, age, age_group,
		        country_id, country_name, country_probability, created_at
		 FROM profiles%s ORDER BY %s %s`,
		qb.whereClause(), qb.sortBy, qb.sortDir,
	)

	rows, err := database.Query(exportSQL, qb.args...)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "export query failed")
		return
	}
	defer rows.Close()

	filename := fmt.Sprintf("profiles_%s.csv", time.Now().UTC().Format("20060102_150405"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

	cw := csv.NewWriter(w)
	cw.Write([]string{
		"id", "name", "gender", "gender_probability", "sample_size",
		"age", "age_group", "country_id", "country_name", "country_probability", "created_at",
	})

	for rows.Next() {
		var p models.Profile
		if err := rows.Scan(
			&p.ID, &p.Name, &p.Gender, &p.GenderProbability, &p.SampleSize,
			&p.Age, &p.AgeGroup, &p.CountryID, &p.CountryName, &p.CountryProbability, &p.CreatedAt,
		); err != nil {
			continue
		}
		cw.Write([]string{
			p.ID, p.Name, p.Gender,
			strconv.FormatFloat(p.GenderProbability, 'f', 4, 64),
			strconv.Itoa(p.SampleSize),
			strconv.Itoa(p.Age),
			p.AgeGroup, p.CountryID, p.CountryName,
			strconv.FormatFloat(p.CountryProbability, 'f', 4, 64),
			p.CreatedAt,
		})
	}
	cw.Flush()
}

// ─── POST /api/profiles ───────────────────────────────────────────────────────

func HandleCreateProfile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		apiError(w, http.StatusBadRequest, "name is required")
		return
	}
	if !hasLetter(name) {
		apiError(w, http.StatusUnprocessableEntity, "name must contain at least one letter")
		return
	}

	database, err := db.Connect()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "service unavailable")
		return
	}

	// Return the existing record rather than erroring on duplicates.
	existing, err := fetchByName(database, name)
	if err == nil {
		respond(w, http.StatusOK, map[string]any{"status": "success", "message": "profile already exists", "data": existing})
		return
	}

	data, err := enrich(name)
	if err != nil {
		apiError(w, http.StatusBadGateway, err.Error())
		return
	}

	profile := models.Profile{
		ID:                 uuid.New().String(),
		Name:               name,
		Gender:             data.Gender,
		GenderProbability:  data.GenderProbability,
		SampleSize:         data.SampleSize,
		Age:                data.Age,
		AgeGroup:           data.AgeGroup,
		CountryID:          data.CountryID,
		CountryName:        data.CountryName,
		CountryProbability: data.CountryProbability,
		CreatedAt:          time.Now().UTC().Format(time.RFC3339),
	}

	_, err = database.Exec(
		`INSERT INTO profiles
		   (id, name, gender, gender_probability, sample_size,
		    age, age_group, country_id, country_name, country_probability, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		profile.ID, profile.Name, profile.Gender, profile.GenderProbability, profile.SampleSize,
		profile.Age, profile.AgeGroup, profile.CountryID, profile.CountryName,
		profile.CountryProbability, profile.CreatedAt,
	)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "failed to save profile")
		return
	}

	respond(w, http.StatusCreated, map[string]any{"status": "success", "data": profile})
}

// ─── GET /api/profiles/{id} ───────────────────────────────────────────────────

func HandleGetProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		apiError(w, http.StatusBadRequest, "profile id is required")
		return
	}

	database, err := db.Connect()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "service unavailable")
		return
	}

	row := database.QueryRow(
		`SELECT id, name, gender, gender_probability, sample_size, age, age_group,
		        country_id, country_name, country_probability, created_at
		 FROM profiles WHERE id = $1`, id,
	)

	profile, err := scanProfile(row)
	if err == sql.ErrNoRows {
		apiError(w, http.StatusNotFound, "profile not found")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "database error")
		return
	}

	respond(w, http.StatusOK, map[string]any{"status": "success", "data": profile})
}

// ─── DELETE /api/profiles/{id} ────────────────────────────────────────────────

func HandleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	database, err := db.Connect()
	if err != nil {
		apiError(w, http.StatusInternalServerError, "service unavailable")
		return
	}

	result, err := database.Exec(`DELETE FROM profiles WHERE id = $1`, id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if n, _ := result.RowsAffected(); n == 0 {
		apiError(w, http.StatusNotFound, "profile not found")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ─── Natural language search ──────────────────────────────────────────────────

// parseNaturalLanguage interprets free-text queries like "young males from Nigeria"
// and returns a pre-populated queryBuilder.
func parseNaturalLanguage(query string) (*queryBuilder, error) {
	lower := strings.ToLower(query)
	words := strings.Fields(lower)

	qb := newQueryBuilder()
	matched := false

	// Gender signals.
	isMale := hasAny(words, "male", "males", "man", "men", "boy", "boys", "he", "his")
	isFemale := hasAny(words, "female", "females", "woman", "women", "girl", "girls", "she", "her")
	if isMale && !isFemale {
		qb.addCondition("gender", "male")
		matched = true
	} else if isFemale && !isMale {
		qb.addCondition("gender", "female")
		matched = true
	}

	// Age group signals.
	switch {
	case hasAny(words, "child", "children", "kid", "kids"):
		qb.addCondition("age_group", "child")
		matched = true
	case hasAny(words, "teen", "teenager", "teenagers", "adolescent", "adolescents", "youth"):
		qb.addCondition("age_group", "teenager")
		matched = true
	case hasAny(words, "adult", "adults"):
		qb.addCondition("age_group", "adult")
		matched = true
	case hasAny(words, "senior", "seniors", "elder", "elderly", "old"):
		qb.addCondition("age_group", "senior")
		matched = true
	}

	// "young" without a more specific group → 16–24.
	if hasAny(words, "young") && !hasAny(words, "teenager", "teen", "child") {
		qb.addIntGTE("age", 16)
		qb.addIntLTE("age", 24)
		matched = true
	}

	// Numeric age bounds: "above 30", "under 40", "between 20 and 35".
	for i, w := range words {
		if (w == "above" || w == "over" || w == "older") && i+1 < len(words) {
			if n, err := strconv.Atoi(words[i+1]); err == nil {
				qb.addIntGTE("age", n)
				matched = true
			}
		}
		if (w == "below" || w == "under" || w == "younger") && i+1 < len(words) {
			if n, err := strconv.Atoi(words[i+1]); err == nil {
				qb.addIntLTE("age", n)
				matched = true
			}
		}
		if w == "between" && i+2 < len(words) {
			lo, e1 := strconv.Atoi(words[i+1])
			hiWord := i + 2
			if hiWord < len(words) && words[hiWord] == "and" {
				hiWord++
			}
			if hiWord < len(words) {
				hi, e2 := strconv.Atoi(words[hiWord])
				if e1 == nil && e2 == nil {
					qb.addIntGTE("age", lo)
					qb.addIntLTE("age", hi)
					matched = true
				}
			}
		}
	}

	// Country signals — check multi-word phrases first, then single words.
	// Both maps are declared upfront so no goto jumps over declarations.
	countryPhrases := map[string]string{
		"south africa": "ZA", "united states": "US", "united kingdom": "GB",
		"dr congo": "CD", "ivory coast": "CI",
	}
	countryWords := map[string]string{
		"nigeria": "NG", "ghana": "GH", "kenya": "KE", "tanzania": "TZ",
		"uganda": "UG", "ethiopia": "ET", "egypt": "EG", "morocco": "MA",
		"senegal": "SN", "cameroon": "CM", "angola": "AO", "sudan": "SD",
		"benin": "BJ", "usa": "US", "uk": "GB", "canada": "CA",
		"france": "FR", "germany": "DE", "brazil": "BR",
		"india": "IN", "china": "CN", "japan": "JP", "australia": "AU",
	}

	countryFound := false
	for phrase, code := range countryPhrases {
		if strings.Contains(lower, phrase) {
			qb.addCondition("country_id", code)
			matched = true
			countryFound = true
			break
		}
	}
	if !countryFound {
		for word, code := range countryWords {
			if hasAny(words, word) {
				qb.addCondition("country_id", code)
				matched = true
				break
			}
		}
	}

	if !matched {
		return nil, fmt.Errorf("could not extract any filters from %q — try something like \"young males from Nigeria\"", query)
	}
	return qb, nil
}

// ─── Small utilities ──────────────────────────────────────────────────────────

func hasAny(words []string, targets ...string) bool {
	set := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		set[t] = struct{}{}
	}
	for _, w := range words {
		if _, ok := set[strings.Trim(w, ".,!?;:")]; ok {
			return true
		}
	}
	return false
}

func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

func fetchByName(database *sql.DB, name string) (*models.Profile, error) {
	row := database.QueryRow(
		`SELECT id, name, gender, gender_probability, sample_size, age, age_group,
		        country_id, country_name, country_probability, created_at
		 FROM profiles WHERE LOWER(name) = LOWER($1)`, name,
	)
	return scanProfile(row)
}
