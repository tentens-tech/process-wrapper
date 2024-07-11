package process

import (
	"database/sql"
	"fmt"
	_ "github.com/go-sql-driver/mysql" // init
	log "github.com/sirupsen/logrus"
	"strings"
)

type SQLProbe struct {
	Queries  []string `yaml:"queries"`
	Contains []string `yaml:"contains"`
	Envs     map[string]string
	db       *sql.DB
}

func (p *SQLProbe) String() string {
	c := strings.Join(p.Contains, ";")
	return fmt.Sprintf("SQL contains %v", c)
}

func (p *SQLProbe) NewDBConnect(dsn string) (*sql.DB, error) {
	var err error
	if p.db == nil {
		log.Printf("Create db connection")
		p.db, err = sql.Open("mysql", dsn)
		if err != nil {
			log.Errorln(err)
			return nil, err
		}
	}
	return p.db, nil
}

func (p *SQLProbe) Run(name string) bool {
	var err error
	var db *sql.DB

	logger := log.WithFields(log.Fields{"name": name})
	logger.Debugf("MySQL probe envs: %+v", p.Envs)

	dsn, ok := p.Envs["MYSQL_DSN"]
	if !ok || dsn == "" {
		logger.Errorf("MYSQL_DSN isn't set")
		return false
	}
	db, err = p.NewDBConnect(dsn)
	if err != nil {
		logger.Errorf("Cannot connect to db, %v", err)
		return false
	}

	for _, query := range p.Queries {
		var responseList []string
		var r bool
		responseList, err = p.SQLQuery(db, query, logger)
		if err != nil {
			logger.Errorf("SQL query error, %v", err)
		}
		for _, e := range p.Contains {
			r = sliceContains(responseList, e)
			if !r {
				logger.Errorf("DB: %v not ready", e)
				return false
			}
		}
	}
	return true
}

func (p *SQLProbe) SQLQuery(db *sql.DB, query string, logger *log.Entry) ([]string, error) {
	var err error
	var results *sql.Rows
	var list []string
	results, err = db.Query(query)
	if err != nil {
		logger.Errorf("Cannot execute sql probe query")
		return nil, err
	}
	for results.Next() {
		var r string
		err = results.Scan(&r)
		if err != nil {
			logger.Errorf("Cannot scan sql probe results, %v", err)
			return nil, err
		}
		list = append(list, r)
	}
	return list, nil
}
