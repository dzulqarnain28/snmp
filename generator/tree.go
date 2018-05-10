package main

import (
	"regexp"
	"sort"
	"strings"

	"github.com/prometheus/common/log"

	"github.com/prometheus/snmp_exporter/config"
)

// Helper to walk MIB nodes.
func walkNode(n *Node, f func(n *Node)) {
	f(n)
	for _, c := range n.Children {
		walkNode(c, f)
	}
}

// Transform the tree.
func prepareTree(nodes *Node) map[string]*Node {
	// Build a map from names and oids to nodes.
	nameToNode := map[string]*Node{}
	walkNode(nodes, func(n *Node) {
		nameToNode[n.Oid] = n
		nameToNode[n.Label] = n
	})

	// Trim down description to first sentance, removing extra whitespace.
	walkNode(nodes, func(n *Node) {
		s := strings.Join(strings.Fields(n.Description), " ")
		n.Description = strings.Split(s, ". ")[0]
	})

	// Fix indexes to "INTEGER" rather than an object name.
	// Example: snSlotsEntry in LANOPTICS-HUB-MIB.
	walkNode(nodes, func(n *Node) {
		indexes := []string{}
		for _, i := range n.Indexes {
			if i == "INTEGER" {
				// Use the TableEntry name.
				indexes = append(indexes, n.Label)
			} else {
				indexes = append(indexes, i)
			}
		}
		n.Indexes = indexes
	})

	// Copy over indexes based on augments.
	walkNode(nodes, func(n *Node) {
		if n.Augments == "" {
			return
		}
		augmented, ok := nameToNode[n.Augments]
		if !ok {
			log.Warnf("Can't find augmenting oid %s for %s", n.Augments, n.Label)
			return
		}
		for _, c := range n.Children {
			c.Indexes = augmented.Indexes
		}
		n.Indexes = augmented.Indexes
	})

	// Copy indexes from table entries down to the entries.
	walkNode(nodes, func(n *Node) {
		if len(n.Indexes) != 0 {
			for _, c := range n.Children {
				c.Indexes = n.Indexes
			}
		}
	})

	// Include both ASCII and UTF-8 in DisplayString, even though DisplayString
	// is technically only ASCII.
	displayStringRe := regexp.MustCompile(`\d+[at]`)

	// Set type on MAC addresses and strings.
	walkNode(nodes, func(n *Node) {
		// RFC 2579
		switch n.Hint {
		case "1x:":
			n.Type = "PhysAddress48"
		}
		if displayStringRe.MatchString(n.Hint) {
			n.Type = "DisplayString"
		}

		// Some MIBs refer to RFC1213 for this, which is too
		// old to have the right hint set.
		if n.TextualConvention == "DisplayString" {
			n.Type = "DisplayString"
		}
	})

	// Promote Opaque Float/Double textual convention to type.
	walkNode(nodes, func(n *Node) {
		if n.TextualConvention == "Float" || n.TextualConvention == "Double" {
			n.Type = n.TextualConvention
		}
	})

	return nameToNode
}

func metricType(t string) (string, bool) {
	switch t {
	case "gauge", "INTEGER", "GAUGE", "TIMETICKS", "UINTEGER", "UNSIGNED32", "INTEGER32":
		return "gauge", true
	case "counter", "COUNTER", "COUNTER64":
		return "counter", true
	case "OctetString", "OCTETSTR", "BITSTRING":
		return "OctetString", true
	case "IpAddr", "IPADDR", "NETADDR":
		return "IpAddr", true
	case "PhysAddress48", "DisplayString", "Float", "Double":
		return t, true
	default:
		// Unsupported type.
		return "", false
	}
}

func metricAccess(a string) bool {
	switch a {
	case "ACCESS_READONLY", "ACCESS_READWRITE", "ACCESS_CREATE", "ACCESS_NOACCESS":
		return true
	default:
		// the others are inaccessible metrics.
		return false
	}
}

// Reduce a set of overlapping OID subtrees.
func minimizeOids(oids []string) []string {
	sort.Strings(oids)
	prevOid := ""
	minimized := []string{}
	for _, oid := range oids {
		if !strings.HasPrefix(oid+".", prevOid) || prevOid == "" {
			minimized = append(minimized, oid)
			prevOid = oid + "."
		}
	}
	return minimized
}

// Search node tree for the longest OID match.
func searchNodeTree(oid string, node *Node) *Node {
	if node == nil || !strings.HasPrefix(oid+".", node.Oid+".") {
		return nil
	}

	for _, child := range node.Children {
		match := searchNodeTree(oid, child)
		if match != nil {
			return match
		}
	}
	return node
}

type oidMetricType uint8

const (
	oidNotFound oidMetricType = iota
	oidScalar
	oidInstance
	oidSubtree
)

// Find node in SNMP MIB tree that represents the metric.
func getMetricNode(oid string, node *Node, nameToNode map[string]*Node) (*Node, oidMetricType) {
	// Check if is a known OID/name.
	n, ok := nameToNode[oid]
	if ok {
		// Known node, check if OID is a valid metric or a subtree.
		_, ok = metricType(n.Type)
		if ok && metricAccess(n.Access) && len(n.Indexes) == 0 {
			return n, oidScalar
		} else {
			return n, oidSubtree
		}
	}

	// Unknown OID/name, search Node tree for longest match.
	n = searchNodeTree(oid, node)
	if n == nil {
		return nil, oidNotFound
	}

	// Table instances must be a valid metric node and have an index.
	_, ok = metricType(n.Type)
	ok = ok && metricAccess(n.Access)
	if !ok || len(n.Indexes) == 0 {
		return nil, oidNotFound
	}
	return n, oidInstance
}

func generateConfigModule(cfg *ModuleConfig, node *Node, nameToNode map[string]*Node) *config.Module {
	out := &config.Module{}
	needToWalk := map[string]struct{}{}
	tableInstances := map[string][]string{}

	// Gather all type overrides for the current module.
	typeOverrides := map[string]string{}
	for name, params := range cfg.Overrides {
		if params.Type == "" {
			continue
		}
		_, ok := nameToNode[name]
		if !ok {
			log.Warnf("Could not find metric '%s' to override type", name)
			continue
		}
		// Type validated at generator configuration.
		typeOverrides[name] = params.Type
	}

	if len(typeOverrides) > 0 {
		// Duplicate node tree.
		node = node.Copy()
		// Rebuild node mapping.
		nameToNode = map[string]*Node{}
		walkNode(node, func(n *Node) {
			nameToNode[n.Oid] = n
			nameToNode[n.Label] = n
		})
		// Apply overrides.
		for name, typ := range typeOverrides {
			n := nameToNode[name]
			n.Type = typ
		}
	}

	// Remove redundant OIDs to be walked.
	toWalk := []string{}
	for _, oid := range cfg.Walk {
		// Resolve name to OID if possible.
		n, ok := nameToNode[oid]
		if ok {
			toWalk = append(toWalk, n.Oid)
		} else {
			toWalk = append(toWalk, oid)
		}
	}
	toWalk = minimizeOids(toWalk)

	// Find all top-level nodes.
	metricNodes := map[*Node]struct{}{}
	for _, oid := range toWalk {
		metricNode, oidType := getMetricNode(oid, node, nameToNode)
		switch oidType {
		case oidNotFound:
			log.Fatalf("Cannot find oid '%s' to walk", oid)
		case oidSubtree:
			needToWalk[oid] = struct{}{}
		case oidInstance:
			// Add a trailing period to the OID to indicate a "Get" instead of a "Walk".
			needToWalk[oid+"."] = struct{}{}
			// Save instance index for lookup.
			index := strings.Replace(oid, metricNode.Oid, "", 1)
			tableInstances[metricNode.Oid] = append(tableInstances[metricNode.Oid], index)
		case oidScalar:
			// Scalar OIDs must be accessed using index 0.
			needToWalk[oid+".0."] = struct{}{}
		}
		metricNodes[metricNode] = struct{}{}
	}
	// Sort the metrics by OID to make the output deterministic.
	metrics := make([]*Node, 0, len(metricNodes))
	for key := range metricNodes {
		metrics = append(metrics, key)
	}
	sort.Slice(metrics, func(i, j int) bool {
		return metrics[i].Oid < metrics[j].Oid
	})

	// Find all the usable metrics.
	for _, metricNode := range metrics {
		walkNode(metricNode, func(n *Node) {
			t, ok := metricType(n.Type)
			if !ok {
				return // Unsupported type.
			}

			if !metricAccess(n.Access) {
				return // Inaccessible metrics.
			}

			metric := &config.Metric{
				Name:    sanitizeLabelName(n.Label),
				Oid:     n.Oid,
				Type:    t,
				Help:    n.Description + " - " + n.Oid,
				Indexes: []*config.Index{},
				Lookups: []*config.Lookup{},
			}
			for _, i := range n.Indexes {
				index := &config.Index{Labelname: i}
				indexNode, ok := nameToNode[i]
				if !ok {
					log.Warnf("Error, can't find index %s for node %s", i, n.Label)
					return
				}
				index.Type, ok = metricType(indexNode.Type)
				if !ok {
					log.Warnf("Error, can't handle index type %s for node %s", indexNode.Type, n.Label)
					return
				}
				index.FixedSize = indexNode.FixedSize
				metric.Indexes = append(metric.Indexes, index)
			}
			out.Metrics = append(out.Metrics, metric)
		})
	}

	// Apply lookups.
	for _, lookup := range cfg.Lookups {
		for _, metric := range out.Metrics {
			for _, index := range metric.Indexes {
				if index.Labelname == lookup.OldIndex {
					if _, ok := nameToNode[lookup.NewIndex]; !ok {
						log.Fatalf("Unknown index '%s'", lookup.NewIndex)
					}
					indexNode := nameToNode[lookup.NewIndex]
					// Avoid leaving the old labelname around.
					index.Labelname = sanitizeLabelName(indexNode.Label)
					typ, ok := metricType(indexNode.Type)
					if !ok {
						log.Fatalf("Unknown index type %s for %s", indexNode.Type, lookup.NewIndex)
					}
					metric.Lookups = append(metric.Lookups, &config.Lookup{
						Labels:    []string{sanitizeLabelName(indexNode.Label)},
						Labelname: sanitizeLabelName(indexNode.Label),
						Type:      typ,
						Oid:       indexNode.Oid,
					})
					// Make sure we walk the lookup OID(s).
					if len(tableInstances[metric.Oid]) > 0 {
						for _, index := range tableInstances[metric.Oid] {
							needToWalk[indexNode.Oid+index+"."] = struct{}{}
						}
					} else {
						needToWalk[indexNode.Oid] = struct{}{}
					}
				}
			}
		}
	}

	// Apply module config overrides to their corresponding metrics.
	for name, params := range cfg.Overrides {
		for _, metric := range out.Metrics {
			if name == metric.Name || name == metric.Oid {
				metric.RegexpExtracts = params.RegexpExtracts
			}
		}
	}

	oids := []string{}
	for k, _ := range needToWalk {
		oids = append(oids, k)
	}
	// Remove redundant OIDs and separate Walk and Get OIDs.
	for _, k := range minimizeOids(oids) {
		if k[len(k)-1:] == "." {
			out.Get = append(out.Get, k[:len(k)-1])
		} else {
			out.Walk = append(out.Walk, k)
		}
	}
	return out
}

var (
	invalidLabelCharRE = regexp.MustCompile(`[^a-zA-Z0-9_]`)
)

func sanitizeLabelName(name string) string {
	return invalidLabelCharRE.ReplaceAllString(name, "_")
}
