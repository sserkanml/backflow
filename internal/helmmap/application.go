package helmmap

import (
	"github.com/sserkanml/backflow/internal/appvalues"
	"github.com/sserkanml/backflow/internal/rendermap"
)

// ApplicationOptions returns the options of the Application (helm.parameters,
// helm.valuesObject, helm.values) that set the fields that could not be mapped,
// one per option, with the fields each one sets. helm is the helm block of the source (at says where it is in the
// Application). A suggested value is given only when the option's own value is
// what the field copies: the live value of the field, or for a part of it (an
// image tag) the new text of that part.
func ApplicationOptions(at appvalues.Where, fields []rendermap.Field, unmapped map[string]bool, helm map[string]any) []appvalues.Option {
	var out []appvalues.Option
	add := func(o appvalues.Option, field string) {
		for i := range out {
			if out[i].Option == o.Option {
				out[i].Fields = append(out[i].Fields, field)
				return
			}
		}
		o.Fields = []string{field}
		out = append(out, o)
	}
	for _, f := range fields {
		if !unmapped[f.Change.Path] {
			continue
		}
		var results []*Result
		for _, found := range []*rendermap.Found{f.Found, f.Fragments} {
			if found == nil {
				continue
			}
			if r, ok := found.Diagnostics.(*Result); ok {
				results = append(results, r)
			}
		}
		live, liveOK := ScalarOf(f.Change.Live)
		for _, r := range results {
			for _, c := range r.Controlled {
				suggested, has := c.NewValue, c.HasNew
				if has && !c.Fragment {
					if !liveOK {
						has = false
					} else {
						suggested = rendermap.RenderValue(live)
					}
				}
				var o appvalues.Option
				var ok bool
				switch c.Layer {
				case LayerParameters:
					o, ok = appvalues.HelmParameter(at, helm, c.Name, suggested, has)
				case LayerValuesObject:
					o, ok = appvalues.HelmValuesObject(at, helm, c.Path, suggested, has)
				case LayerValues:
					o, ok = appvalues.HelmValues(at, c.Path), true
				}
				if ok {
					add(o, f.Change.Path)
				}
			}
		}
	}
	return out
}
