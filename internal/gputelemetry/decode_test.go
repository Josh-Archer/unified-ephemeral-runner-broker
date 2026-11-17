package gputelemetry

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var selection = Selection{"node", "UUID", "desktop", "GPU-one"}
var now = time.Unix(1700000000, 0).UTC()

const valid = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"node":"desktop","UUID":"GPU-one","extra":"retained"},"value":[1700000000,"12"]}]}}`

func reason(t *testing.T, err error, want Reason) {
	t.Helper()
	var r *Rejection
	if !errors.As(err, &r) || r.Reason != want {
		t.Fatalf("got %v want %s", err, want)
	}
}
func TestDecode(t *testing.T) {
	sample, err := Decode([]byte(valid), selection, now, time.Second)
	if err != nil || sample.Value != 12 || !sample.At.Equal(now) || sample.Timestamp != "1700000000" || sample.Labels["extra"] != "retained" {
		t.Fatalf("%+v %v", sample, err)
	}
	cases := []struct {
		name, body string
		want       Reason
	}{
		{"empty vector", `{"status":"success","data":{"resultType":"vector","result":[]}}`, MissingSeries},
		{"scalar", strings.Replace(valid, "vector", "scalar", 1), InvalidInput},
		{"aggregate", strings.Replace(valid, `"UUID":"GPU-one",`, "", 1), InvalidInput},
		{"wrong GPU", strings.ReplaceAll(valid, "GPU-one", "GPU-two"), MissingSeries},
		{"wrong node", strings.ReplaceAll(valid, "desktop", "other"), MissingSeries},
		{"nan", strings.Replace(valid, `"12"`, `"NaN"`, 1), InvalidInput},
		{"infinity", strings.Replace(valid, `"12"`, `"+Inf"`, 1), InvalidInput},
		{"negative", strings.Replace(valid, `"12"`, `"-1"`, 1), InvalidInput},
		{"missing value", strings.Replace(valid, `,"12"`, "", 1), InvalidInput},
		{"numeric value", strings.Replace(valid, `"12"`, `12`, 1), InvalidInput},
		{"stale", strings.Replace(valid, "1700000000", "1699999998", 1), StaleSample},
		{"future", strings.Replace(valid, "1700000000", "1700000000.000000001", 1), StaleSample},
		{"subnanosecond", strings.Replace(valid, "1700000000", "1700000000.0000000001", 1), InvalidInput},
		{"overflow", strings.Replace(valid, "1700000000", "1e1000", 1), InvalidInput},
		{"quoted timestamp", strings.Replace(valid, "1700000000", `"1700000000"`, 1), InvalidInput},
		{"warnings", strings.Replace(valid, `"status":`, `"warnings":["partial"],"status":`, 1), UpstreamFailure},
		{"error", strings.Replace(valid, `"status":`, `"error":"private raw error","status":`, 1), UpstreamFailure},
		{"duplicate label", strings.Replace(valid, `"UUID":`, `"UUID":"GPU-other","UUID":`, 1), InvalidInput},
		{"duplicate status", strings.Replace(valid, `"status":`, `"status":"error","status":`, 1), InvalidInput},
		{"trailing document", valid + `{}`, InvalidInput},
		{"invalid json", "{", InvalidInput},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Decode([]byte(c.body), selection, now, time.Second)
			reason(t, err, c.want)
		})
	}
}
func TestMultipleDevicesAndAmbiguity(t *testing.T) {
	var response map[string]any
	if err := json.Unmarshal([]byte(valid), &response); err != nil {
		t.Fatal(err)
	}
	data := response["data"].(map[string]any)
	rows := data["result"].([]any)
	data["result"] = append(rows, rows[0])
	body, _ := json.Marshal(response)
	_, err := Decode(body, selection, now, time.Second)
	reason(t, err, AmbiguousSeries)
	var other map[string]any
	json.Unmarshal([]byte(valid), &other)
	row := other["data"].(map[string]any)["result"].([]any)[0].(map[string]any)
	row["metric"].(map[string]any)["UUID"] = "GPU-two"
	data["result"] = append(rows, row)
	body, _ = json.Marshal(response)
	if _, err := Decode(body, selection, now, time.Second); err != nil {
		t.Fatal(err)
	}
}
func TestFreshnessAndConfiguration(t *testing.T) {
	for _, s := range []Selection{{}, {"node", "node", "desktop", "GPU-one"}} {
		_, err := Decode([]byte(valid), s, now, time.Second)
		reason(t, err, InvalidInput)
	}
	for _, age := range []time.Duration{0, -1} {
		_, err := Decode([]byte(valid), selection, now, age)
		reason(t, err, InvalidInput)
	}
	_, err := Decode([]byte(valid), selection, time.Time{}, time.Second)
	reason(t, err, InvalidInput)
	if _, err := Decode([]byte(valid), selection, now.Add(time.Second), time.Second); err != nil {
		t.Fatal(err)
	}
	_, err = Decode([]byte(valid), selection, now.Add(time.Second+1), time.Second)
	reason(t, err, StaleSample)
}
func TestBundle(t *testing.T) {
	inputs := map[Metric][]byte{}
	for _, m := range required {
		inputs[m] = []byte(valid)
	}
	out, err := DecodeBundle(inputs, selection, now, time.Second)
	if err != nil || len(out) != 8 {
		t.Fatalf("%v", err)
	}
	for _, m := range required {
		saved := inputs[m]
		delete(inputs, m)
		_, err := DecodeBundle(inputs, selection, now, time.Second)
		reason(t, err, IncompleteBundle)
		inputs[m] = saved
	}
	inputs[ProtectedLatency] = []byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`)
	_, err = DecodeBundle(inputs, selection, now, time.Second)
	reason(t, err, MissingSeries)
	inputs[ProtectedLatency] = []byte(valid)
	inputs[ProtectedErrors] = []byte(strings.Replace(valid, "1700000000", "1699999998", 1))
	_, err = DecodeBundle(inputs, selection, now, time.Second)
	reason(t, err, StaleSample)
}
func FuzzDecode(f *testing.F) {
	f.Add([]byte(valid))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, body []byte) {
		sample, err := Decode(body, selection, now, time.Second)
		if err == nil {
			if sample.Labels["UUID"] != "GPU-one" || sample.Labels["node"] != "desktop" || sample.At.After(now) || sample.At.Before(now.Add(-time.Second)) {
				t.Fatal("invalid accepted sample")
			}
		}
	})
}

func TestExactSchemaKeysAndNumericEvidence(t *testing.T) {
	for _, key := range []string{"status", "data", "resultType", "result", "metric", "value"} {
		upper := strings.ToUpper(key[:1]) + key[1:]
		body := strings.Replace(valid, `"`+key+`":`, `"`+upper+`":`, 1)
		_, err := Decode([]byte(body), selection, now, time.Second)
		reason(t, err, InvalidInput)
	}
	body := strings.Replace(valid, `"status":"success"`, `"status":"error","Status":"success"`, 1)
	_, err := Decode([]byte(body), selection, now, time.Second)
	reason(t, err, InvalidInput)
	for _, value := range []string{"1.0000000000000001", "9007199254740993", "18446744073709551615"} {
		body := strings.Replace(valid, `"12"`, `"`+value+`"`, 1)
		sample, err := Decode([]byte(body), selection, now, time.Second)
		if err != nil || sample.ValueText != value {
			t.Fatalf("numeric evidence lost: %+v %v", sample, err)
		}
	}
}
