package metrics

import (
	"fmt"
	"io"
	"strconv"
	"time"
)

type Sample struct {
	Name, Environment, Asset, Instance, Node, Interface string
	Value                                               float64
}

func Write(w io.Writer, samples []Sample, observed time.Time) error {
	for _, sample := range samples {
		labels := "environment=" + strconv.Quote(sample.Environment) + ",asset=" + strconv.Quote(sample.Asset) + ",instance=" + strconv.Quote(sample.Instance) + ",node=" + strconv.Quote(sample.Node)
		if sample.Interface != "" {
			labels += ",interface=" + strconv.Quote(sample.Interface)
		}
		if _, err := fmt.Fprintf(w, "netlab_%s{%s} %s %d\n", sample.Name, labels, strconv.FormatFloat(sample.Value, 'g', -1, 64), observed.UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}
