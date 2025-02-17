package badger

import "github.com/prometheus/client_golang/prometheus"

// NewBadgerCollector returns a prometheus Collector for Badger metrics from expvar.
func (bdb *badgerDB) metricsCollector() {
	collector := prometheus.NewExpvarCollector(map[string]*prometheus.Desc{
		"badger_v3_disk_reads_total": prometheus.NewDesc(
			"badger_disk_reads_total",
			"Number of cumulative reads by Badger",
			nil, nil,
		),
		"badger_v3_disk_writes_total": prometheus.NewDesc(
			"badger_disk_writes_total",
			"Number of cumulative writes by Badger",
			nil, nil,
		),
		"badger_v3_read_bytes": prometheus.NewDesc(
			"badger_read_bytes",
			"Number of cumulative bytes read by Badger",
			nil, nil,
		),
		"badger_v3_written_bytes": prometheus.NewDesc(
			"badger_written_bytes",
			"Number of cumulative bytes written by Badger",
			nil, nil,
		),
		"badger_v3_lsm_level_gets_total": prometheus.NewDesc(
			"badger_lsm_level_gets_total",
			"Total number of LSM gets",
			[]string{"level"}, nil,
		),
		"badger_v3_lsm_bloom_hits_total": prometheus.NewDesc(
			"badger_lsm_bloom_hits_total",
			"Total number of LSM bloom hits",
			[]string{"level"}, nil,
		),
		"badger_v3_gets_total": prometheus.NewDesc(
			"badger_gets_total",
			"Total number of gets",
			nil, nil,
		),
		"badger_v3_puts_total": prometheus.NewDesc(
			"badger_puts_total",
			"Total number of puts",
			nil, nil,
		),
		"badger_v3_blocked_puts_total": prometheus.NewDesc(
			"badger_blocked_puts_total",
			"Total number of blocked puts",
			nil, nil,
		),
		"badger_v3_memtable_gets_total": prometheus.NewDesc(
			"badger_memtable_gets_total",
			"Total number of memtable gets",
			nil, nil,
		),
		"badger_v3_lsm_size_bytes": prometheus.NewDesc(
			"badger_lsm_size_bytes",
			"Size of the LSM in bytes",
			[]string{"dir"}, nil,
		),
		"badger_v3_vlog_size_bytes": prometheus.NewDesc(
			"badger_vlog_size_bytes",
			"Size of the value log in bytes",
			[]string{"dir"}, nil,
		),
		"badger_v3_pending_writes_total": prometheus.NewDesc(
			"badger_pending_writes_total",
			"Total number of pending writes",
			[]string{"dir"}, nil,
		),
		"badger_v3_compactions_current": prometheus.NewDesc(
			"badger_compactions_current",
			"Number of tables being actively compacted",
			nil, nil,
		),
	})

	bdb.opts.promRegistry.MustRegister(collector)
}
