package main

import (
	"flag"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/heptiolabs/healthcheck"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/xanzy/go-gitlab"

	"gopkg.in/yaml.v2"
)

type config struct {
	Gitlab struct {
		URL   string
		Token string
	}

	PollingIntervalSeconds int `yaml:"polling_interval_seconds"`
	Projects               []project
	Wildcards              []wildcard
}

type client struct {
	*gitlab.Client
	config *config
}

type project struct {
	Name string
	Refs []string
}

type wildcard struct {
	Search string
	Owner  struct {
		Name string
		Kind string
	}
	Refs []string
}

var (
	listenAddress = flag.String("listen-address", ":8080", "Listening address")
	configPath    = flag.String("config", "/etc/config.yml", "Config file path")
)

var (
	timeSinceLastRun = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gitlab_ci_pipeline_time_since_last_run_seconds",
			Help: "Elapsed time since most recent GitLab CI pipeline run.",
		},
		[]string{"project", "ref"},
	)

	lastRunDuration = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gitlab_ci_pipeline_last_run_duration_seconds",
			Help: "Duration of last pipeline run",
		},
		[]string{"project", "ref"},
	)
	runCount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gitlab_ci_pipeline_run_count",
			Help: "GitLab CI pipeline run count",
		},
		[]string{"project", "ref"},
	)

	status = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gitlab_ci_pipeline_status",
			Help: "GitLab CI pipeline current status",
		},
		[]string{"project", "ref", "status"},
	)
)

func (c *client) getProject(name string) *gitlab.Project {
	p, _, err := c.Projects.GetProject(name, &gitlab.GetProjectOptions{})
	if err != nil {
		log.Fatalf("Unable to fetch project '%v' from the GitLab API : %v", name, err.Error())
		os.Exit(1)
	}
	return p
}

func (c *client) fetchProjectsFromWildcards() {
	for _, w := range c.config.Wildcards {
		for _, p := range c.listProjects(&w) {
			c.config.Projects = append(c.config.Projects, p)
		}
	}
}

func (c *client) listProjects(w *wildcard) (projects []project) {
	log.Printf("-> Listing all projects using search pattern : '%s' with owner '%s' (%s)", w.Search, w.Owner.Name, w.Owner.Kind)

	trueVal := true
	falseVal := false
	var gps []*gitlab.Project
	var err error

	switch w.Owner.Kind {
	case "user":
		gps, _, err = c.Projects.ListUserProjects(
			w.Owner.Name,
			&gitlab.ListProjectsOptions{
				Archived: &falseVal,
				Simple:   &trueVal,
				Search:   &w.Search,
			},
		)
	case "group":
		gps, _, err = c.Groups.ListGroupProjects(
			w.Owner.Name,
			&gitlab.ListGroupProjectsOptions{
				Archived: &falseVal,
				Simple:   &trueVal,
				Search:   &w.Search,
			},
		)
	default:
		log.Fatalf("Invalid owner kind '%s' must be either 'user' or 'group'", w.Owner.Kind)
		os.Exit(1)
	}

	if err != nil {
		log.Fatalf("Unable to list projects with search pattern '%s' from the GitLab API : %v", w.Search, err.Error())
		os.Exit(1)
	}

	for _, gp := range gps {
		log.Printf("-> Found project : %s", gp.PathWithNamespace)
		projects = append(
			projects,
			project{
				Name: gp.PathWithNamespace,
				Refs: w.Refs,
			},
		)
	}

	return
}

func (c *client) pollProject(p project) {
	for _, ref := range p.Refs {
		go c.pollProjectRef(p.Name, ref)
	}
}

func (c *client) pollProjectRef(name, ref string) {
	gp := c.getProject(name)
	log.Printf("--> Polling ID: %v | %v:%v", gp.ID, name, ref)

	var lastPipeline *gitlab.Pipeline

	for {
		pipelines, _, _ := c.Pipelines.ListProjectPipelines(gp.ID, &gitlab.ListProjectPipelinesOptions{Ref: gitlab.String(ref)})
		if lastPipeline == nil || lastPipeline.ID != pipelines[0].ID || lastPipeline.Status != pipelines[0].Status {
			if lastPipeline != nil {
				runCount.WithLabelValues(name, ref).Inc()
			}

			if len(pipelines) > 0 {
				lastPipeline, _, _ = c.Pipelines.GetPipeline(gp.ID, pipelines[0].ID)
				lastRunDuration.WithLabelValues(name, ref).Set(float64(lastPipeline.Duration))

				for _, s := range []string{"success", "failed", "running"} {
					if s == lastPipeline.Status {
						status.WithLabelValues(name, ref, s).Set(1)
					} else {
						status.WithLabelValues(name, ref, s).Set(0)
					}
				}
			} else {
				log.Printf("Could not find any pipeline for %s:%s", name, ref)
			}
		}

		if lastPipeline != nil {
			timeSinceLastRun.WithLabelValues(name, ref).Set(float64(time.Since(*lastPipeline.CreatedAt).Round(time.Second).Seconds()))
		}

		time.Sleep(time.Duration(c.config.PollingIntervalSeconds) * time.Second)
	}
}

func (c *client) pollProjects() {
	c.fetchProjectsFromWildcards()
	log.Printf("-> %d project(s) configured with a total of %d ref(s)", len(c.config.Projects), c.sumTotalRefs())
	for _, p := range c.config.Projects {
		c.pollProject(p)
	}
}

func (c *client) sumTotalRefs() (refs int) {
	refs = 0
	for _, p := range c.config.Projects {
		refs += len(p.Refs)
	}
	return
}

func init() {
	prometheus.MustRegister(timeSinceLastRun)
	prometheus.MustRegister(lastRunDuration)
	prometheus.MustRegister(runCount)
	prometheus.MustRegister(status)
}

func main() {
	flag.Parse()

	var config config

	configFile, err := ioutil.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("Couldn't open config file : %v", err.Error())
		os.Exit(1)
	}

	err = yaml.Unmarshal(configFile, &config)
	if err != nil {
		log.Fatalf("Unable to parse config file: %v", err.Error())
		os.Exit(1)
	}

	if len(config.Projects) < 1 && len(config.Wildcards) < 1 {
		log.Fatalf("You need to configure at least one project/wildcard to poll, none given")
		os.Exit(1)
	}

	log.Printf("-> Starting exporter")
	log.Printf("-> Polling %v every %vs", config.Gitlab.URL, config.PollingIntervalSeconds)

	c := &client{
		gitlab.NewClient(nil, config.Gitlab.Token),
		&config,
	}

	c.SetBaseURL(config.Gitlab.URL)
	c.pollProjects()

	// Configure liveness and readiness probes
	health := healthcheck.NewHandler()
	health.AddLivenessCheck("goroutine-threshold", healthcheck.GoroutineCountCheck(len(config.Projects)+20))
	health.AddReadinessCheck("gitlab-reachable", healthcheck.HTTPGetCheck(config.Gitlab.URL+"/users/sign_in", 5*time.Second))

	// Expose the registered metrics via HTTP
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", health.LiveEndpoint)
	mux.HandleFunc("/health/ready", health.ReadyEndpoint)
	mux.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(*listenAddress, mux))
}
