package elasticsearch

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/plugins/outputs"
	"gopkg.in/olivere/elastic.v5"
)

type Elasticsearch struct {
	URLs                []string `toml:"urls"`
	IndexName           string
	Username            string
	Password            string
	EnableSniffer       bool
	Timeout             internal.Duration
	HealthCheckInterval internal.Duration
	ManageTemplate      bool
	TemplateName        string
	OverwriteTemplate   bool
	Client              *elastic.Client
}

var sampleConfig = `
  ## The full HTTP endpoint URL for your Elasticsearch instance
  ## Multiple urls can be specified as part of the same cluster,
  ## this means that only ONE of the urls will be written to each interval.
  urls = [ "http://node1.es.example.com:9200" ] # required.
  ## Elasticsearch client timeout, defaults to "5s" if not set. 
  timeout = "5s"
  ## Set to true to ask Elasticsearch a list of all cluster nodes,
  ## thus it is not necessary to list all nodes in the urls config option.
  enable_sniffer = false
  ## Set the interval to check if the Elasticsearch nodes are available
  ## Setting to "0s" will disable the health check (not recommended in production)
  health_check_interval = "10s"
  ## HTTP basic authentication details (eg. when using Shield)
  # username = "telegraf"
  # password = "mypassword"

  ## Index Config
  ## The target index for metrics (Elasticsearch will create if it not exists).
  ## You can use the date specifiers below to create indexes per time frame.
  ## The metric timestamp will be used to decide the destination index name
  # %Y - year (2016)
  # %y - last two digits of year (00..99)
  # %m - month (01..12)
  # %d - day of month (e.g., 01)
  # %H - hour (00..23)
  index_name = "telegraf-%Y.%m.%d" # required.

  ## Template Config
  ## Set to true if you want telegraf to manage its index template.
  ## If enabled it will create a recommended index template for telegraf indexes
  manage_template = true
  ## The template name used for telegraf indexes
  template_name = "telegraf"
  ## Set to true if you want telegraf to overwrite an existing template
  overwrite_template = false
`

func (a *Elasticsearch) Connect() error {
	if a.URLs == nil || a.IndexName == "" {
		return fmt.Errorf("Elasticsearch urls or index_name is not defined")
	}

	ctx, cancel := context.WithTimeout(context.Background(), a.Timeout.Duration)
	defer cancel()

	var clientOptions []elastic.ClientOptionFunc

	clientOptions = append(clientOptions,
		elastic.SetSniff(a.EnableSniffer),
		elastic.SetURL(a.URLs...),
		elastic.SetHealthcheckInterval(a.HealthCheckInterval.Duration),
	)

	if a.Username != "" && a.Password != "" {
		clientOptions = append(clientOptions,
			elastic.SetBasicAuth(a.Username, a.Password),
		)
	}

	if a.HealthCheckInterval.Duration == 0 {
		clientOptions = append(clientOptions,
			elastic.SetHealthcheck(false),
		)
		log.Printf("D! Elasticsearch output: disabling health check")
	}

	client, err := elastic.NewClient(clientOptions...)

	if err != nil {
		return err
	}

	// check for ES version on first node
	esVersion, err := client.ElasticsearchVersion(a.URLs[0])

	if err != nil {
		return fmt.Errorf("Elasticsearch version check failed: %s", err)
	}

	// quit if ES version is not supported
	i, err := strconv.Atoi(strings.Split(esVersion, ".")[0])
	if err != nil || i < 5 {
		return fmt.Errorf("Elasticsearch version not supported: %s", esVersion)
	}

	log.Println("I! Elasticsearch version: " + esVersion)

	a.Client = client

	if a.ManageTemplate {
		err := a.manageTemplate(ctx)
		if err != nil {
			return err
		}
	}

	return nil
}

func (a *Elasticsearch) Write(metrics []telegraf.Metric) error {
	if len(metrics) == 0 {
		return nil
	}

	bulkRequest := a.Client.Bulk()

	for _, metric := range metrics {
		var name = metric.Name()
		indexName := ""
		// index name has to be re-evaluated each time for telegraf
		// to send the metric to the correct time-based index
		tags := metric.Tags()
		m := make(map[string]interface{})

		if device_id, ok := tags["device_id"]; ok {
			indexName = fmt.Sprintf("device_%s", device_id)
			m["device_id"] = tags["device_id"]
		}
		if deploy_id,ok := tags["deploy_id"]; ok{
			indexName = fmt.Sprintf("deploy_%s", deploy_id)
			m["deploy_id"] = tags["deploy_id"]
		}

		m["@timestamp"] = metric.Time()
		m["tenant_id"] = tags["tenant_id"]

		m[name] = metric.Fields()

		bulkRequest.Add(elastic.NewBulkIndexRequest().
			Index(indexName).
			Type(name).
			Doc(m))

	}

	ctx, cancel := context.WithTimeout(context.Background(), a.Timeout.Duration)
	defer cancel()

	res, err := bulkRequest.Do(ctx)

	if err != nil {
		return fmt.Errorf("Error sending bulk request to Elasticsearch: %s", err)
	}

	if res.Errors {
		for id, err := range res.Failed() {
			log.Printf("E! Elasticsearch indexing failure, id: %d, error: %s, caused by: %s, %s", id, err.Error.Reason, err.Error.CausedBy["reason"], err.Error.CausedBy["type"])
		}
		return fmt.Errorf("W! Elasticsearch failed to index %d metrics", len(res.Failed()))
	}

	return nil

}

func (a *Elasticsearch) manageTemplate(ctx context.Context) error {
	if a.TemplateName == "" {
		return fmt.Errorf("Elasticsearch template_name configuration not defined")
	}

	templateExists, errExists := a.Client.IndexTemplateExists(a.TemplateName).Do(ctx)

	if errExists != nil {
		return fmt.Errorf("Elasticsearch template check failed, template name: %s, error: %s", a.TemplateName, errExists)
	}

	templatePattern := a.IndexName + "*"

	if strings.Contains(a.IndexName, "%") {
		templatePattern = a.IndexName[0:strings.Index(a.IndexName, "%")] + "*"
	}

	if (a.OverwriteTemplate) || (!templateExists) {
		// Create or update the template
		tmpl := fmt.Sprintf(`
			{
				"template":"%s",
				"settings": {
					"index": {
						"refresh_interval": "10s",
						"mapping.total_fields.limit": 5000
					}
				},
				"mappings" : {
					"_default_" : {
						"_all": { "enabled": false	  },
						"properties" : {
							"@timestamp" : { "type" : "date" },
							"measurement_name" : { "type" : "keyword" }
						},
						"dynamic_templates": [
							{
								"tags": {
									"match_mapping_type": "string",
									"path_match": "tag.*",
									"mapping": {
										"ignore_above": 512,
										"type": "keyword"
									}
								}
							},
							{
								"metrics_long": {
									"match_mapping_type": "long",
									"mapping": {
										"type": "float",
										"index": false
									}
								}
							},
							{
								"metrics_double": {
									"match_mapping_type": "double",
									"mapping": {
										"type": "float",
										"index": false
									}
								}
							},
							{
								"text_fields": {
									"match": "*",
									"mapping": {
										"norms": false
									}
								}
							}
						]
					}
				}
			}`, templatePattern)
		_, errCreateTemplate := a.Client.IndexPutTemplate(a.TemplateName).BodyString(tmpl).Do(ctx)

		if errCreateTemplate != nil {
			return fmt.Errorf("Elasticsearch failed to create index template %s : %s", a.TemplateName, errCreateTemplate)
		}

		log.Printf("D! Elasticsearch template %s created or updated\n", a.TemplateName)

	} else {

		log.Println("D! Found existing Elasticsearch template. Skipping template management")

	}
	return nil
}

func (a *Elasticsearch) GetIndexName(indexName string, eventTime time.Time) string {
	if strings.Contains(indexName, "%") {
		var dateReplacer = strings.NewReplacer(
			"%Y", eventTime.UTC().Format("2006"),
			"%y", eventTime.UTC().Format("06"),
			"%m", eventTime.UTC().Format("01"),
			"%d", eventTime.UTC().Format("02"),
			"%H", eventTime.UTC().Format("15"),
		)

		indexName = dateReplacer.Replace(indexName)
	}

	return indexName

}

func (a *Elasticsearch) SampleConfig() string {
	return sampleConfig
}

func (a *Elasticsearch) Description() string {
	return "Configuration for Elasticsearch to send metrics to."
}

func (a *Elasticsearch) Close() error {
	a.Client = nil
	return nil
}

func init() {
	outputs.Add("elasticsearch", func() telegraf.Output {
		return &Elasticsearch{
			Timeout:             internal.Duration{Duration: time.Second * 5},
			HealthCheckInterval: internal.Duration{Duration: time.Second * 10},
		}
	})
}
