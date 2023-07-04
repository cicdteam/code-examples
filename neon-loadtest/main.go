package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"database/sql"
	_ "github.com/lib/pq"

	neon "github.com/cicdteam/neon-go-client"
	"github.com/deepmap/oapi-codegen/pkg/securityprovider"

	"go.uber.org/zap/zapcore"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	// where Neon API resides
	neonApiBaseURL = "https://console.stage.neon.tech/api/v2"

	// db first connection timeout
	dbConnTimeout = "1m0s"
	// SQL Query used to place some load to CPUS in compute-nodes
	heavySqlQuery = "SELECT SUM(t.generate_series) FROM (SELECT * FROM generate_series(1, 10000000)) t;"
	// duration for heavy SQL query
	heavySqlQueryDuration = "3m0s"
	// pause between heavy SQL query series
	heavySqlQueryPause = "1m0s"
	// how many queries run in parallel
	parallelism = 32

	// project creation loop delay, milliseconds
	loopDelayMS = 1000

	// projects name prefix
	namePrefix = "loadtest-"
)

var (
	// count of computes to run
	count int
	// Neon region used for tests
	region string
	// duration for load-tests
	duration string
	// place heavy workload or just start computes
	load bool
	// autoscaling settings
	minCU float64
	maxCU float64

	// delete all projcts with namePrefix
	cleanup bool

	// provisioner and region where create projects
	provisioner neon.Provisioner = neon.K8sNeonvm
)

func main() {

	flag.IntVar(&count, "count", 3, "number of Neon projects to create")
	flag.StringVar(&region, "region", "aws-eu-west-1", "Neon region where to run computes")
	flag.StringVar(&duration, "duration", "5m0s", "duration of test")
	flag.BoolVar(&load, "load", true, "run heavy sql workload")
	flag.Float64Var(&minCU, "min-cu", 0.25, "minimal autoscaling compute units")
	flag.Float64Var(&maxCU, "max-cu", 7.0, "maximal autoscaling compute units")
	flag.BoolVar(&cleanup, "cleanup", false, fmt.Sprintf("delete all projects with name prefix %s", namePrefix))

	// define logging options
	opts := zap.Options{
		Development:     true,
		StacktraceLevel: zapcore.Level(zapcore.PanicLevel),
		TimeEncoder:     zapcore.ISO8601TimeEncoder,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// define logger
	logger := zap.New(zap.UseFlagOptions(&opts))

	// define context with logger
	ctx := log.IntoContext(context.Background(), logger)

	// get Neon api key and contruct bearer
	apiKey := os.Getenv("NEON_API_KEY")
	if len(apiKey) == 0 {
		logger.Error(errors.New("variable NEON_API_KEY not found or empty"), "error")
		os.Exit(1)
	}
	// See: https://swagger.io/docs/specification/authentication/bearer-authentication/
	bearer, err := securityprovider.NewSecurityProviderBearerToken(apiKey)
	if err != nil {
		logger.Error(err, "api key setup error")
		os.Exit(1)
	}

	// get Neon client
	client, err := neon.NewClient(neonApiBaseURL, neon.WithRequestEditorFn(bearer.Intercept))
	if err != nil {
		logger.Error(err, "neon client error")
		os.Exit(1)
	}

	// delete all projects with prefix in names
	if cleanup {
		found, err := ListProjects(ctx, client)
		if err != nil {
			logger.Error(err, "project list error")
			os.Exit(1)
		}
		deleted := 0
		for _, f := range found {
			if strings.HasPrefix(f.Name, namePrefix) {
				_, err := client.DeleteProject(ctx, f.Id)
				if err != nil {
					logger.Error(err, "project deletion error")
				} else {
					logger.Info("project deleted", "name", f.Name, "id", f.Id)
					deleted++
				}
			}
		}
		logger.Info("cleanup", "projects found", len(found), "projects deleted", deleted)
		os.Exit(0)
	}

	// parse and check test duration arg
	d, err := time.ParseDuration(duration)
	if err != nil {
		logger.Error(err, "duration not parsed", "duration", duration)
		os.Exit(1)
	}

	logger.Info("staring", "count", count, "region", region, "duration", duration, "load", load, "minCU", minCU, "maxCU", maxCU)
	var wg sync.WaitGroup
	for loop := 1; loop <= count; loop++ {
		wg.Add(1)
		go runLoadTest(ctx, &wg, client, fmt.Sprintf("%s%04d", namePrefix, loop), d, load, minCU, maxCU)
		time.Sleep(time.Millisecond * loopDelayMS)
	}
	wg.Wait()

	logger.Info("load tests finished")
}

func runLoadTest(ctx context.Context, wg *sync.WaitGroup, client *neon.Client, name string, d time.Duration, load bool, mincu, maxcu float64) {
	defer wg.Done()

	var err error
	log := log.FromContext(ctx)

	pause, _ := time.ParseDuration(heavySqlQueryPause)

	p, err := CreateProject(ctx, client, name, mincu, maxcu)
	if err != nil {
		log.Error(err, "fail to create project")
		return
	}
	log.Info("project created", "name", p.Project.Name, "id", p.Project.Id)

	s := time.Now()
	for {
		// check if we should finish load tests
		if time.Since(s) >= d {
			break
		}

		if load {
			log.Info("starting workload", "project", p.Project.Name, "duration", heavySqlQueryDuration)
		}
		if err = runSqlWorkload(ctx, p, load); err != nil {
			log.Error(err, "fail to run sql workload")
			return
		}

		// sleep some random time (from heavySqlQueryPause to 2*heavySqlQueryPause) between wokload series
		sleep := pause + time.Duration(rand.Intn(int(pause.Seconds())))*time.Second
		if load {
			log.Info("pause workload", "project", p.Project.Name, "duration", sleep)
		}
		time.Sleep(sleep)
	}

	err = DeleteProject(ctx, client, p)
	if err != nil {
		log.Error(err, "fail to delete project")
		return
	}
	log.Info("project deleted", "name", p.Project.Name)

	return
}

// place heavy SQL query to project
func runSqlWorkload(ctx context.Context, p *ProjectInfo, load bool) error {
	log := log.FromContext(ctx)
	var wg sync.WaitGroup

	d, _ := time.ParseDuration(heavySqlQueryDuration)

	db, err := sql.Open("postgres", p.ConnectionUris[0].ConnectionUri)
	if err != nil {
		return err
	}
	defer db.Close()

	// first - check db connetion
	t := time.Now()
	if err := dbPing(ctx, db); err != nil {
		log.Error(err, "db ping error", "project", p.Project.Name)
		return err
	}
	log.Info("db ready", "project", p.Project.Name, "latency", time.Since(t).Round(time.Millisecond))

	// run SQL queries in parallel
	for i := 1; i <= parallelism; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			// run query in the loop until we reach heavySqlQueryDuration
			start := time.Now()
			stmt, err := db.PrepareContext(ctx, heavySqlQuery)
			if err != nil {
				log.Error(err, "Statement prepare error", "project", p.Project.Name)
				return
			}
			defer stmt.Close()
			stmtSimple, err := db.PrepareContext(ctx, "SELECT version()")
			if err != nil {
				log.Error(err, "Simple statement prepare error", "project", p.Project.Name)
				return
			}
			defer stmtSimple.Close()

			for {
				// check if we should stop test
				if time.Since(start) >= d {
					break
				}

				// create separate context for query with timeout
				dbCtx, dbCancel := context.WithTimeout(ctx, d)
				defer dbCancel()

				// exec simple query, we not interested in results
				_, err = stmtSimple.ExecContext(dbCtx)
				if err != nil && err.Error() != "pq: canceling statement due to user request" {
					log.Error(err, "SELECT version() error", "project", p.Project.Name)
					continue
				}

				if load {
					// exec heavy query, we not interested in results
					_, err := stmt.ExecContext(dbCtx)
					if err != nil && err.Error() != "pq: canceling statement due to user request" {
						log.Error(err, "sql query error", "project", p.Project.Name)
						continue
					}
				} else {
					time.Sleep(time.Second)
				}
			}
		}(i)
	}
	wg.Wait()

	return nil
}

// Ping the database to verify DSN is valid and the server accessible.
func dbPing(ctx context.Context, db *sql.DB) error {
	t, _ := time.ParseDuration(dbConnTimeout)
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return err
	}

	return nil
}

type ProjectInfo struct {
	Branch         neon.Branch              `json:"branch"`
	ConnectionUris []neon.ConnectionDetails `json:"connection_uris"`
	Databases      []neon.Database          `json:"databases"`
	Endpoints      []neon.Endpoint          `json:"endpoints"`
	Operations     []neon.Operation         `json:"operations"`
	Project        neon.Project             `json:"project"`
	Roles          []neon.Role              `json:"roles"`
}

func CreateProject(ctx context.Context, client *neon.Client, name string, minCU, maxCU float64) (*ProjectInfo, error) {
	body := neon.CreateProjectJSONRequestBody{
		Project: struct {
			AutoscalingLimitMaxCu *neon.ComputeUnit `json:"autoscaling_limit_max_cu,omitempty"`
			AutoscalingLimitMinCu *neon.ComputeUnit `json:"autoscaling_limit_min_cu,omitempty"`
			Branch                *struct {
				// DatabaseName The database name. If not specified, the default database name will be used.
				DatabaseName *string `json:"database_name,omitempty"`

				// Name The branch name. If not specified, the default branch name will be used.
				Name *string `json:"name,omitempty"`

				// RoleName The role name. If not specified, the default role name will be used.
				RoleName *string `json:"role_name,omitempty"`
			} `json:"branch,omitempty"`

			// DefaultEndpointSettings A collection of settings for a Neon endpoint
			DefaultEndpointSettings *neon.DefaultEndpointSettings `json:"default_endpoint_settings,omitempty"`

			// HistoryRetentionSeconds The number of seconds to retain the point-in-time restore (PITR) backup history for this project.
			// The default is 604800 seconds (7 days).
			HistoryRetentionSeconds *int64 `json:"history_retention_seconds,omitempty"`

			// Name The project name
			Name *string `json:"name,omitempty"`

			// PgVersion The major PostgreSQL version number. Currently supported version are `14` and `15`.
			PgVersion *neon.PgVersion `json:"pg_version,omitempty"`

			// Provisioner The Neon compute provisioner. Select the `k8s-neonvm` provisioner to enable autoscaling.
			Provisioner *neon.Provisioner `json:"provisioner,omitempty"`

			// RegionId The region identifier. Refer to our [Regions](https://neon.tech/docs/introduction/regions) documentation for supported regions. Values are specified in this format: `aws-us-east-1`
			RegionId *string                   `json:"region_id,omitempty"`
			Settings *neon.ProjectSettingsData `json:"settings,omitempty"`

			// StorePasswords Whether or not passwords are stored for roles in the Neon project. Storing passwords facilitates access to Neon features that require authorization.
			StorePasswords *bool `json:"store_passwords,omitempty"`
		}{
			Name:                  &name,
			AutoscalingLimitMinCu: &[]neon.ComputeUnit{float32(minCU)}[0],
			AutoscalingLimitMaxCu: &[]neon.ComputeUnit{float32(maxCU)}[0],
			Provisioner:           &provisioner,
			RegionId:              &region,
		},
	}
	resp, err := client.CreateProject(ctx, body)
	if err != nil {
		return nil, err
	}
	parsed, err := neon.ParseCreateProjectResponse(resp)
	if err != nil {
		return nil, err
	}
	json201 := parsed.JSON201
	p := ProjectInfo(*json201)

	return &p, nil
}

func ListProjects(ctx context.Context, client *neon.Client) ([]neon.ProjectListItem, error) {
	limit := 100
	cursor := ""
	items := []neon.ProjectListItem{}
	for {
		resp, err := client.ListProjects(ctx, &neon.ListProjectsParams{Cursor: &cursor, Limit: &limit})
		if err != nil {
			return items, err
		}
		parced, err := neon.ParseListProjectsResponse(resp)
		if err != nil {
			return items, err
		}
		projects := parced.JSON200.Projects
		if len(projects) == 0 {
			break
		}
		cursor = parced.JSON200.Pagination.Cursor
		items = append(items, projects...)
	}

	return items, nil
}

func DeleteProject(ctx context.Context, client *neon.Client, p *ProjectInfo) error {
	_, err := client.DeleteProject(ctx, p.Project.Id)
	return err
}
