// MIT License
//
// Copyright (c) 2022 faceair
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package drain

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/hashicorp/golang-lru/simplelru"
	"golang.org/x/exp/slices"
)

type Config struct {
	maxNodeDepth    int
	LogClusterDepth int
	SimTh           float64
	MaxChildren     int
	ExtraDelimiters []string
	MaxClusters     int
	ParamString     string
}

type LogCluster struct {
	id     int
	Size   int
	Tokens []string

	Samples []string
	Volume  Volume
}

const (
	timeResolution = int64(time.Second * 10)
	maxSamples     = 10

	defaultVolumeSize = 500
)

func (c *LogCluster) getTemplate() string {
	return strings.Join(c.Tokens, " ")
}

func (c *LogCluster) String() string {
	return c.getTemplate()
}

func truncateTimestamp(ts int64) int64 { return ts - ts%timeResolution }

type Volume struct {
	Values [][2]int64 // 0 timestamp, 1 count.
}

func initVolume(ts int64) Volume {
	v := Volume{Values: make([][2]int64, 1, defaultVolumeSize)}
	v.Values[0] = [2]int64{ts, 1}
	return v
}

// ForRange returns a new Volume with only the values
// in the given range [start:end).
func (x *Volume) ForRange(start, end int64) *Volume {
	if len(x.Values) == 0 {
		// Should not be the case.
		return new(Volume)
	}
	first := x.Values[0][0]
	last := x.Values[len(x.Values)-1][0]
	if start >= end || first >= end || last < start {
		return new(Volume)
	}
	var lo int
	if start > first {
		lo = sort.Search(len(x.Values), func(i int) bool {
			return x.Values[i][0] >= start
		})
	}
	hi := len(x.Values)
	if end < last {
		hi = sort.Search(len(x.Values), func(i int) bool {
			return x.Values[i][0] >= end
		})
	}
	return &Volume{
		Values: x.Values[lo:hi],
	}
}

func (x *Volume) Matches() int64 {
	var m int64
	for i := range x.Values {
		m += x.Values[i][1]
	}
	return m
}

func (x *Volume) Add(ts int64) {
	t := truncateTimestamp(ts)
	first := x.Values[0][0] // can't be empty
	last := x.Values[len(x.Values)-1][0]
	switch {
	case last == t:
		// Should be the most common case.
		x.Values[len(x.Values)-1][1]++
	case first > t:
		// Prepend.
		x.Values = slices.Grow(x.Values, 1)
		copy(x.Values[1:], x.Values)
		x.Values[0] = [2]int64{t, 1}
	case last < t:
		// Append.
		x.Values = append(x.Values, [2]int64{t, 1})
	default:
		// Find with binary search and update.
		index := sort.Search(len(x.Values), func(i int) bool {
			return x.Values[i][1] >= t
		})
		if index < len(x.Values) && x.Values[index][1] == t {
			x.Values[index][1]++
		} else {
			x.Values = slices.Insert(x.Values, index, [2]int64{t, 1})
		}
	}
}

func (c *LogCluster) append(content string, ts int64) {
	c.Volume.Add(ts)
	// TODO: Should we sample lines randomly? Keep last N?
	if len(c.Samples) < maxSamples {
		c.Samples = append(c.Samples, content)
	}
}

func createLogClusterCache(maxSize int) *LogClusterCache {
	if maxSize == 0 {
		maxSize = math.MaxInt
	}
	cache, _ := simplelru.NewLRU(maxSize, nil)
	return &LogClusterCache{
		cache: cache,
	}
}

type LogClusterCache struct {
	cache simplelru.LRUCache
}

func (c *LogClusterCache) Values() []*LogCluster {
	values := make([]*LogCluster, 0)
	for _, key := range c.cache.Keys() {
		if value, ok := c.cache.Peek(key); ok {
			values = append(values, value.(*LogCluster))
		}
	}
	return values
}

func (c *LogClusterCache) Set(key int, cluster *LogCluster) {
	c.cache.Add(key, cluster)
}

func (c *LogClusterCache) Iterate(fn func(*LogCluster) bool) {
	for _, key := range c.cache.Keys() {
		if value, ok := c.cache.Peek(key); ok {
			if !fn(value.(*LogCluster)) {
				return
			}
		}
	}
}

func (c *LogClusterCache) Get(key int) *LogCluster {
	cluster, ok := c.cache.Get(key)
	if !ok {
		return nil
	}
	return cluster.(*LogCluster)
}

func createNode() *Node {
	return &Node{
		keyToChildNode: make(map[string]*Node),
		clusterIDs:     make([]int, 0),
	}
}

type Node struct {
	keyToChildNode map[string]*Node
	clusterIDs     []int
}

func DefaultConfig() *Config {
	return &Config{
		LogClusterDepth: 4,
		SimTh:           0.4,
		MaxChildren:     100,
		ParamString:     "<*>",
	}
}

func New(config *Config) *Drain {
	if config.LogClusterDepth < 3 {
		panic("depth argument must be at least 3")
	}
	config.maxNodeDepth = config.LogClusterDepth - 2

	d := &Drain{
		config:      config,
		rootNode:    createNode(),
		idToCluster: createLogClusterCache(config.MaxClusters),
	}
	return d
}

type Drain struct {
	config          *Config
	rootNode        *Node
	idToCluster     *LogClusterCache
	clustersCounter int
}

func (d *Drain) Clusters() []*LogCluster {
	return d.idToCluster.Values()
}

func (d *Drain) Iterate(fn func(*LogCluster) bool) {
	d.idToCluster.Iterate(fn)
}

func (d *Drain) Train(content string, ts int64) *LogCluster {
	contentTokens := d.getContentAsTokens(content)

	matchCluster := d.treeSearch(d.rootNode, contentTokens, d.config.SimTh, false)
	// Match no existing log cluster
	if matchCluster == nil {
		d.clustersCounter++
		clusterID := d.clustersCounter
		matchCluster = &LogCluster{
			Tokens: contentTokens,
			id:     clusterID,
			Size:   1,

			Samples: []string{content},
			Volume:  initVolume(ts),
		}
		d.idToCluster.Set(clusterID, matchCluster)
		d.addSeqToPrefixTree(d.rootNode, matchCluster)
	} else {
		newTemplateTokens := d.createTemplate(contentTokens, matchCluster.Tokens)
		matchCluster.Tokens = newTemplateTokens
		matchCluster.Size++
		matchCluster.append(content, ts)
		// Touch cluster to update its state in the cache.
		d.idToCluster.Get(matchCluster.id)
	}
	return matchCluster
}

// Match against an already existing cluster. Match shall be perfect (sim_th=1.0). New cluster will not be created as a result of this call, nor any cluster modifications.
func (d *Drain) Match(content string) *LogCluster {
	contentTokens := d.getContentAsTokens(content)
	matchCluster := d.treeSearch(d.rootNode, contentTokens, 1.0, true)
	return matchCluster
}

func (d *Drain) getContentAsTokens(content string) []string {
	content = strings.TrimSpace(content)
	for _, extraDelimiter := range d.config.ExtraDelimiters {
		content = strings.Replace(content, extraDelimiter, " ", -1)
	}
	return strings.Split(content, " ")
}

func (d *Drain) treeSearch(rootNode *Node, tokens []string, simTh float64, includeParams bool) *LogCluster {
	tokenCount := len(tokens)

	// at first level, children are grouped by token (word) count
	curNode, ok := rootNode.keyToChildNode[strconv.Itoa(tokenCount)]

	// no template with same token count yet
	if !ok {
		return nil
	}

	// handle case of empty log string - return the single cluster in that group
	if tokenCount == 0 {
		return d.idToCluster.Get(curNode.clusterIDs[0])
	}

	// find the leaf node for this log - a path of nodes matching the first N tokens (N=tree depth)
	curNodeDepth := 1
	for _, token := range tokens {
		// at max depth
		if curNodeDepth >= d.config.maxNodeDepth {
			break
		}

		// this is last token
		if curNodeDepth == tokenCount {
			break
		}

		keyToChildNode := curNode.keyToChildNode
		curNode, ok = keyToChildNode[token]
		if !ok { // no exact next token exist, try wildcard node
			curNode, ok = keyToChildNode[d.config.ParamString]
		}
		if !ok { // no wildcard node exist
			return nil
		}
		curNodeDepth++
	}

	// get best match among all clusters with same prefix, or None if no match is above sim_th
	cluster := d.fastMatch(curNode.clusterIDs, tokens, simTh, includeParams)
	return cluster
}

// fastMatch Find the best match for a log message (represented as tokens) versus a list of clusters
func (d *Drain) fastMatch(clusterIDs []int, tokens []string, simTh float64, includeParams bool) *LogCluster {
	var matchCluster, maxCluster *LogCluster

	maxSim := -1.0
	maxParamCount := -1
	for _, clusterID := range clusterIDs {
		// Try to retrieve cluster from cache with bypassing eviction
		// algorithm as we are only testing candidates for a match.
		cluster := d.idToCluster.Get(clusterID)
		if cluster == nil {
			continue
		}
		curSim, paramCount := d.getSeqDistance(cluster.Tokens, tokens, includeParams)
		if curSim > maxSim || (curSim == maxSim && paramCount > maxParamCount) {
			maxSim = curSim
			maxParamCount = paramCount
			maxCluster = cluster
		}
	}
	if maxSim >= simTh {
		matchCluster = maxCluster
	}
	return matchCluster
}

func (d *Drain) getSeqDistance(seq1, seq2 []string, includeParams bool) (float64, int) {
	if len(seq1) != len(seq2) {
		panic("seq1 seq2 be of same length")
	}

	simTokens := 0
	paramCount := 0
	for i := range seq1 {
		token1 := seq1[i]
		token2 := seq2[i]
		if token1 == d.config.ParamString {
			paramCount++
		} else if token1 == token2 {
			simTokens++
		}
	}
	if includeParams {
		simTokens += paramCount
	}
	retVal := float64(simTokens) / float64(len(seq1))
	return retVal, paramCount
}

func (d *Drain) addSeqToPrefixTree(rootNode *Node, cluster *LogCluster) {
	tokenCount := len(cluster.Tokens)
	tokenCountStr := strconv.Itoa(tokenCount)

	firstLayerNode, ok := rootNode.keyToChildNode[tokenCountStr]
	if !ok {
		firstLayerNode = createNode()
		rootNode.keyToChildNode[tokenCountStr] = firstLayerNode
	}
	curNode := firstLayerNode

	// handle case of empty log string
	if tokenCount == 0 {
		curNode.clusterIDs = append(curNode.clusterIDs, cluster.id)
		return
	}

	currentDepth := 1
	for _, token := range cluster.Tokens {
		// if at max depth or this is last token in template - add current log cluster to the leaf node
		if (currentDepth >= d.config.maxNodeDepth) || currentDepth >= tokenCount {
			// clean up stale clusters before adding a new one.
			newClusterIDs := make([]int, 0, len(curNode.clusterIDs))
			for _, clusterID := range curNode.clusterIDs {
				if d.idToCluster.Get(clusterID) != nil {
					newClusterIDs = append(newClusterIDs, clusterID)
				}
			}
			newClusterIDs = append(newClusterIDs, cluster.id)
			curNode.clusterIDs = newClusterIDs
			break
		}

		// if token not matched in this layer of existing tree.
		if _, ok = curNode.keyToChildNode[token]; !ok {
			// if token not matched in this layer of existing tree.
			if !d.hasNumbers(token) {
				if _, ok = curNode.keyToChildNode[d.config.ParamString]; ok {
					if len(curNode.keyToChildNode) < d.config.MaxChildren {
						newNode := createNode()
						curNode.keyToChildNode[token] = newNode
						curNode = newNode
					} else {
						curNode = curNode.keyToChildNode[d.config.ParamString]
					}
				} else {
					if len(curNode.keyToChildNode)+1 < d.config.MaxChildren {
						newNode := createNode()
						curNode.keyToChildNode[token] = newNode
						curNode = newNode
					} else if len(curNode.keyToChildNode)+1 == d.config.MaxChildren {
						newNode := createNode()
						curNode.keyToChildNode[d.config.ParamString] = newNode
						curNode = newNode
					} else {
						curNode = curNode.keyToChildNode[d.config.ParamString]
					}
				}
			} else {
				if _, ok = curNode.keyToChildNode[d.config.ParamString]; !ok {
					newNode := createNode()
					curNode.keyToChildNode[d.config.ParamString] = newNode
					curNode = newNode
				} else {
					curNode = curNode.keyToChildNode[d.config.ParamString]
				}
			}
		} else {
			// if the token is matched
			curNode = curNode.keyToChildNode[token]
		}

		currentDepth++
	}
}

func (d *Drain) hasNumbers(s string) bool {
	for _, c := range s {
		if unicode.IsNumber(c) {
			return true
		}
	}
	return false
}

func (d *Drain) createTemplate(seq1, seq2 []string) []string {
	if len(seq1) != len(seq2) {
		panic("seq1 seq2 be of same length")
	}
	retVal := make([]string, len(seq2))
	copy(retVal, seq2)
	for i := range seq1 {
		if seq1[i] != seq2[i] {
			retVal[i] = d.config.ParamString
		}
	}
	return retVal
}
