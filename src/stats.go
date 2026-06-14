package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"sync"
	"time"
)

type UsageRecord struct {
	Time       time.Time
	Service    string
	Model      string
	DurationMs int64
	Prompt     int
	Completion int
	Total      int
}

type StatKey struct {
	Date    string
	Service string
	Model   string
}

type StatValue struct {
	Requests   int
	Prompt     int
	Completion int
	Total      int
}

var statsMap = make(map[StatKey]*StatValue)
var statsMua sync.Mutex

const StatsFile = "usage_stats.csv"

func aggregatorWorker() {
	loadStats()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	dirty := false

	for {
		select {
		case record := <-usageLogChan:
			statsMua.Lock()
			date := record.Time.Format("2006-01-02")
			key := StatKey{Date: date, Service: record.Service, Model: record.Model}
			val, exists := statsMap[key]
			if !exists {
				val = &StatValue{}
				statsMap[key] = val
			}
			val.Requests++
			val.Prompt += record.Prompt
			val.Completion += record.Completion
			val.Total += record.Total
			statsMua.Unlock()
			dirty = true
		case <-ticker.C:
			if dirty {
				saveStats()
				dirty = false
			}
		}
	}
}

func loadStats() {
	f, err := os.Open(StatsFile)
	if err != nil {
		return
	}
	defer f.Close()

	reader := csv.NewReader(f)
	if _, err := reader.Read(); err != nil {
		return
	}

	statsMua.Lock()
	defer statsMua.Unlock()

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(record) < 7 {
			continue
		}
		date := record[0]
		svc := record[1]
		model := record[2]
		var reqs, p, c, t int
		fmt.Sscanf(record[3], "%d", &reqs)
		fmt.Sscanf(record[4], "%d", &p)
		fmt.Sscanf(record[5], "%d", &c)
		fmt.Sscanf(record[6], "%d", &t)
		statsMap[StatKey{date, svc, model}] = &StatValue{
			Requests: reqs, Prompt: p, Completion: c, Total: t,
		}
	}
}

func saveStats() {
	statsMua.Lock()
	defer statsMua.Unlock()

	tempFile := StatsFile + ".tmp"
	f, err := os.Create(tempFile)
	if err != nil {
		log.Printf("[ERR] Failed to create temp stats file: %v", err)
		return
	}
	defer f.Close()

	writer := csv.NewWriter(f)
	writer.Write([]string{"Date", "Service", "Model", "Requests", "Prompt", "Completion", "Total"})

	var keys []StatKey
	for k := range statsMap {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Date != keys[j].Date {
			return keys[i].Date > keys[j].Date
		}
		if keys[i].Service != keys[j].Service {
			return keys[i].Service < keys[j].Service
		}
		return keys[i].Model < keys[j].Model
	})

	for _, k := range keys {
		v := statsMap[k]
		writer.Write([]string{
			k.Date, k.Service, k.Model,
			fmt.Sprintf("%d", v.Requests),
			fmt.Sprintf("%d", v.Prompt),
			fmt.Sprintf("%d", v.Completion),
			fmt.Sprintf("%d", v.Total),
		})
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		log.Printf("[ERR] CSV Write Error: %v", err)
		return
	}
	f.Close()

	if err := os.Rename(tempFile, StatsFile); err != nil {
		log.Printf("[ERR] Failed to rename stats file: %v", err)
		os.Remove(tempFile)
	}
}
