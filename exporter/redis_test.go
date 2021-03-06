package exporter

/*
  to run the tests with redis running on anything but localhost:6379 use
  $ go test   --redis.addr=<host>:<port>

  for html coverage report run
  $ go test -coverprofile=coverage.out  && go tool cover -html=coverage.out
*/

import (
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"bytes"
	"flag"
	"github.com/garyburd/redigo/redis"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

var (
	redisAddr = flag.String("redis.addr", "localhost:6379", "Address of the test instance, without `redis://`")

	keys             = []string{}
	keysExpiring     = []string{}
	ts               = int32(time.Now().Unix())
	defaultRedisHost = RedisHost{}

	dbNumStr     = "11"
	dbNumStrFull = fmt.Sprintf("db%s", dbNumStr)
)

const (
	TestSetName = "test-set"
)

func setupDBKeys(t *testing.T) error {

	c, err := redis.DialURL(defaultRedisHost.Addrs[0])
	if err != nil {
		t.Errorf("couldn't setup redis, err: %s ", err)
		return err
	}
	defer c.Close()

	_, err = c.Do("SELECT", dbNumStr)
	if err != nil {
		t.Errorf("couldn't setup redis, err: %s ", err)
		return err
	}

	for _, key := range keys {
		_, err = c.Do("SET", key, "1234.56")
		if err != nil {
			t.Errorf("couldn't setup redis, err: %s ", err)
			return err
		}
	}

	// setting to expire in 300 seconds, should be plenty for a test run
	for _, key := range keysExpiring {
		_, err = c.Do("SETEX", key, "300", "1234.56")
		if err != nil {
			t.Errorf("couldn't setup redis, err: %s ", err)
			return err
		}
	}

	c.Do("SADD", TestSetName, "test-val-1")
	c.Do("SADD", TestSetName, "test-val-2")

	time.Sleep(time.Millisecond * 50)

	return nil
}

func deleteKeysFromDB(t *testing.T) error {

	c, err := redis.DialURL(defaultRedisHost.Addrs[0])
	if err != nil {
		t.Errorf("couldn't setup redis, err: %s ", err)
		return err
	}
	defer c.Close()

	_, err = c.Do("SELECT", dbNumStr)
	if err != nil {
		t.Errorf("couldn't setup redis, err: %s ", err)
		return err
	}

	for _, key := range keys {
		c.Do("DEL", key)
	}

	for _, key := range keysExpiring {
		c.Do("DEL", key)
	}

	c.Do("DEL", TestSetName)

	return nil
}

func TestHostVariations(t *testing.T) {
	for _, prefix := range []string{"", "redis://", "tcp://"} {
		addr := prefix + *redisAddr
		host := RedisHost{Addrs: []string{addr}}
		e, _ := NewRedisExporter(host, "test", "")

		scrapes := make(chan scrapeResult, 10000)
		e.scrape(scrapes)
		found := 0
		for range scrapes {
			found++
		}

		if found == 0 {
			t.Errorf("didn't find any scrapes for host: %s", addr)
		}
	}
}

func TestCountingKeys(t *testing.T) {

	e, _ := NewRedisExporter(defaultRedisHost, "test", "")

	scrapes := make(chan scrapeResult, 10000)
	e.scrape(scrapes)

	var keysTestDB float64
	for s := range scrapes {
		if s.Name == "db_keys" && s.DB == dbNumStrFull {
			keysTestDB = s.Value
			break
		}
	}

	setupDBKeys(t)
	defer deleteKeysFromDB(t)

	scrapes = make(chan scrapeResult, 1000)
	e.scrape(scrapes)

	// +1 for the one SET key
	want := keysTestDB + float64(len(keys)) + float64(len(keysExpiring)) + 1

	for s := range scrapes {
		if s.Name == "db_keys" && s.DB == dbNumStrFull {
			if want != s.Value {
				t.Errorf("values not matching, %f != %f", keysTestDB, s.Value)
			}
			break
		}
	}

	deleteKeysFromDB(t)
	scrapes = make(chan scrapeResult, 10000)
	e.scrape(scrapes)

	for s := range scrapes {
		if s.Name == "db_keys" && s.DB == dbNumStrFull {
			if keysTestDB != s.Value {
				t.Errorf("values not matching, %f != %f", keysTestDB, s.Value)
			}
			break
		}
		if s.Name == "db_avg_ttl_seconds" && s.DB == dbNumStrFull {
			if keysTestDB != s.Value {
				t.Errorf("values not matching, %f != %f", keysTestDB, s.Value)
			}
			break
		}
	}
}

func TestExporterMetrics(t *testing.T) {

	e, _ := NewRedisExporter(defaultRedisHost, "test", "")

	setupDBKeys(t)
	defer deleteKeysFromDB(t)

	scrapes := make(chan scrapeResult, 10000)
	e.scrape(scrapes)

	e.setMetrics(scrapes)

	want := 25
	if len(e.metrics) < want {
		t.Errorf("need moar metrics, found: %d, want: %d", len(e.metrics), want)
	}

	wantKeys := []string{
		"db_keys",
		"db_avg_ttl_seconds",
		"used_cpu_sys",
		"loading_dump_file", // testing renames
	}

	for _, k := range wantKeys {
		if _, ok := e.metrics[k]; !ok {
			t.Errorf("missing metrics key: %s", k)
		}
	}
}

func TestExporterValues(t *testing.T) {

	e, _ := NewRedisExporter(defaultRedisHost, "test", "")

	setupDBKeys(t)
	defer deleteKeysFromDB(t)

	scrapes := make(chan scrapeResult, 10000)
	e.scrape(scrapes)

	wantValues := map[string]float64{
		"db_keys_total":          float64(len(keys)+len(keysExpiring)) + 1, // + 1 for the SET key
		"db_expiring_keys_total": float64(len(keysExpiring)),
	}

	for s := range scrapes {
		if wantVal, ok := wantValues[s.Name]; ok {
			if dbNumStrFull == s.DB && wantVal != s.Value {
				t.Errorf("values not matching, %f != %f", wantVal, s.Value)
			}
		}
	}
}

type tstData struct {
	db                        string
	stats                     string
	keysTotal, keysEx, avgTTL float64
	ok                        bool
}

func TestKeyspaceStringParser(t *testing.T) {
	tsts := []tstData{
		{db: "xxx", stats: "", ok: false},
		{db: "xxx", stats: "keys=1,expires=0,avg_ttl=0", ok: false},
		{db: "db0", stats: "xxx", ok: false},
		{db: "db1", stats: "keys=abcd,expires=0,avg_ttl=0", ok: false},
		{db: "db2", stats: "keys=1234=1234,expires=0,avg_ttl=0", ok: false},

		{db: "db3", stats: "keys=abcde,expires=0", ok: false},
		{db: "db3", stats: "keys=213,expires=xxx", ok: false},
		{db: "db3", stats: "keys=123,expires=0,avg_ttl=zzz", ok: false},

		{db: "db0", stats: "keys=1,expires=0,avg_ttl=0", keysTotal: 1, keysEx: 0, avgTTL: 0, ok: true},
	}

	for _, tst := range tsts {
		if kt, kx, ttl, ok := parseDBKeyspaceString(tst.db, tst.stats); true {

			if ok != tst.ok {
				t.Errorf("failed for: db:%s stats:%s", tst.db, tst.stats)
				continue
			}

			if ok && (kt != tst.keysTotal || kx != tst.keysEx || ttl != tst.avgTTL) {
				t.Errorf("values not matching, db:%s stats:%s   %f %f %f", tst.db, tst.stats, kt, kx, ttl)
			}
		}
	}
}

func TestKeyValuesAndSizes(t *testing.T) {

	e, _ := NewRedisExporter(defaultRedisHost, "test", dbNumStrFull+"="+url.QueryEscape(keys[0]))

	setupDBKeys(t)
	defer deleteKeysFromDB(t)

	chM := make(chan prometheus.Metric)
	go func() {
		e.Collect(chM)
		close(chM)
	}()

	want := map[string]bool{"test_key_size": false, "test_key_value": false}

	for m := range chM {
		switch m.(type) {
		case prometheus.Gauge:
			for k := range want {
				if strings.Contains(m.Desc().String(), k) {
					want[k] = true
				}
			}
		default:
			log.Printf("default: m: %#v", m)
		}
	}
	for k, v := range want {
		if !v {
			t.Errorf("didn't find %s", k)
		}

	}
}

func TestCommandStats(t *testing.T) {

	e, _ := NewRedisExporter(defaultRedisHost, "test", dbNumStrFull+"="+url.QueryEscape(keys[0]))

	setupDBKeys(t)
	defer deleteKeysFromDB(t)

	chM := make(chan prometheus.Metric)
	go func() {
		e.Collect(chM)
		close(chM)
	}()

	want := map[string]bool{"test_command_call_duration_seconds_count": false, "test_command_call_duration_seconds_sum": false}

	for m := range chM {
		switch m.(type) {
		case prometheus.Gauge:
			for k := range want {
				if strings.Contains(m.Desc().String(), k) {
					want[k] = true
				}
			}
		default:
			log.Printf("default: m: %#v", m)
		}
	}
	for k, v := range want {
		if !v {
			t.Errorf("didn't find %s", k)
		}

	}
}

func TestHTTPEndpoint(t *testing.T) {

	e, _ := NewRedisExporter(defaultRedisHost, "test", dbNumStrFull+"="+url.QueryEscape(keys[0]))

	setupDBKeys(t)
	defer deleteKeysFromDB(t)
	prometheus.MustRegister(e)

	http.Handle("/metrics", prometheus.Handler())
	go http.ListenAndServe("127.0.0.1:9121", nil)
	time.Sleep(time.Second)
	resp, err := http.Get("http://127.0.0.1:9121/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	tests := []string{
		`test_connected_clients`,
		`test_commands_processed_total`,
		`test_key_size`,
	}
	for _, test := range tests {
		if !bytes.Contains(body, []byte(test)) {
			t.Errorf("want metrics to include %q, have:\n%s", test, body)
		}
	}
}

func TestNonExistingHost(t *testing.T) {

	rr := RedisHost{Addrs: []string{"unix:///tmp/doesnt.exist"}}
	e, _ := NewRedisExporter(rr, "test", "")

	chM := make(chan prometheus.Metric)

	go func() {
		e.Collect(chM)
		close(chM)
	}()

	want := map[string]float64{"test_exporter_last_scrape_error": 1.0, "test_exporter_scrapes_total": 1.0}

	for m := range chM {

		descString := m.Desc().String()

		switch m.(type) {
		case prometheus.Gauge:

			for k := range want {
				if strings.Contains(descString, k) {

					g := &dto.Metric{}
					m.Write(g)

					val := 0.0

					if g.GetGauge() != nil {
						val = *g.GetGauge().Value
					} else if g.GetCounter() != nil {
						val = *g.GetCounter().Value
					} else {
						continue
					}
					if val == want[k] {
						want[k] = -1.0
					}
				}
			}

		default:
			log.Printf("default: m: %#v", m)
		}

	}
	for k, v := range want {
		if v > 0 {
			t.Errorf("didn't find %s", k)
		}
	}
}

func init() {
	for _, n := range []string{"john", "paul", "ringo", "george"} {
		key := fmt.Sprintf("key:%s-%d", n, ts)
		keys = append(keys, key)
	}

	for _, n := range []string{"A.J.", "Howie", "Nick", "Kevin", "Brian"} {
		key := fmt.Sprintf("key:exp-%s-%d", n, ts)
		keysExpiring = append(keysExpiring, key)
	}

	flag.Parse()
	addrs := strings.Split(*redisAddr, ",")
	if len(addrs) == 0 || len(addrs[0]) == 0 {
		log.Fatal("Invalid parameter --redis.addr")
	}
	log.Printf("Using redis addrs: %#v", addrs)

	defaultRedisHost = RedisHost{Addrs: []string{"redis://" + *redisAddr}}
}
