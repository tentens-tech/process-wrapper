package process

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql" // init
	log "github.com/sirupsen/logrus"
)

const SQLProbeTimeout = 10 * time.Second

type SQLProbe struct {
	Queries  []string `yaml:"queries"`
	Contains []string `yaml:"contains"`
	Envs     map[string]string
	db       *sql.DB
	dsn      string
}

func (p *SQLProbe) String() string {
	c := strings.Join(p.Contains, ";")
	return fmt.Sprintf("SQL contains %v", c)
}

// NewDBConnect returns a cached connection pool, recreating it if the DSN changed
func (p *SQLProbe) NewDBConnect(dsn string) (*sql.DB, error) {
	if p.db != nil && p.dsn == dsn {
		return p.db, nil
	}
	if p.db != nil {
		_ = p.db.Close()
		p.db = nil
	}
	log.Printf("Create db connection")
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	// A probe needs a single connection; don't keep it forever
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)
	p.db, p.dsn = db, dsn
	return db, nil
}

func (p *SQLProbe) Run(name string) bool {
	logger := log.WithFields(log.Fields{"name": name})

	dsn := p.Envs["MYSQL_DSN"]
	if dsn == "" {
		logger.Errorf("MYSQL_DSN isn't set")
		return false
	}
	db, err := p.NewDBConnect(dsn)
	if err != nil {
		logger.Errorf("Cannot connect to db, %v", err)
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), SQLProbeTimeout)
	defer cancel()
	for _, query := range p.Queries {
		responseList, err := p.SQLQuery(ctx, db, query)
		if err != nil {
			logger.Errorf("SQL probe query error, %v", err)
			return false
		}
		for _, e := range p.Contains {
			if !sliceContains(responseList, e) {
				logger.Errorf("DB: %v not ready", e)
				return false
			}
		}
	}
	return true
}

// SQLQuery returns the first column of every row
func (p *SQLProbe) SQLQuery(ctx context.Context, db *sql.DB, query string) ([]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("cannot execute sql probe query: %w", err)
	}
	defer rows.Close()

	var list []string
	for rows.Next() {
		var r sql.NullString
		if err = rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("cannot scan sql probe results: %w", err)
		}
		list = append(list, r.String)
	}
	return list, rows.Err()
}
