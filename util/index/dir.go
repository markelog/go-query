package index

import (
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"sync"

	iq "github.com/rekki/go-query"
	"github.com/rekki/go-query/util/analyzer"
	"github.com/rekki/go-query/util/common"
	spec "github.com/rekki/go-query/util/go_query_dsl"
)

type FDCache struct {
	fdCache   map[string]*os.File
	maxOpenFD int
	sync.RWMutex
}

func NewFDCache(n int) *FDCache {
	return &FDCache{maxOpenFD: n, fdCache: map[string]*os.File{}}
}

func (x *FDCache) Close() {
	x.Lock()
	defer x.Unlock()

	for _, fd := range x.fdCache {
		_ = fd.Close()
	}
}

func (x *FDCache) ComputeIfAbsent(fn string, c func(fn string) (*os.File, error)) (*os.File, error) {
	x.RLock()
	f, ok := x.fdCache[fn]
	if !ok {
		x.RUnlock()
		f, err := c(fn)

		if err != nil {
			return nil, err
		}

		x.Lock()
		overriden, ok := x.fdCache[fn]
		if ok {
			f.Close()
			f = overriden
		} else {
			if len(x.fdCache) > x.maxOpenFD {
				for _, fd := range x.fdCache {
					_ = fd.Close()
				}
				x.fdCache = map[string]*os.File{}
			}
			x.fdCache[fn] = f
		}
		x.Unlock()
		return f, nil
	}
	x.RUnlock()
	return f, nil
}

type FileDescriptorCache interface {
	ComputeIfAbsent(fn string, c func(fn string) (*os.File, error)) (*os.File, error)
	Close()
}

type DirIndex struct {
	perField          map[string]*analyzer.Analyzer
	root              string
	fdCache           FileDescriptorCache
	TotalNumberOfDocs int
}

func NewDirIndex(root string, fdCache FileDescriptorCache, perField map[string]*analyzer.Analyzer) *DirIndex {
	if perField == nil {
		perField = map[string]*analyzer.Analyzer{}
	}

	return &DirIndex{TotalNumberOfDocs: 1, root: root, fdCache: fdCache, perField: perField}
}

func termCleanup(s string) string {
	return common.ReplaceNonAlphanumericWith(s, '_')
}

func (d *DirIndex) add(fn string, did int32) error {
	var err error
	f, err := d.fdCache.ComputeIfAbsent(fn, func(_s string) (*os.File, error) {
		f, err := os.OpenFile(fn, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		return f, nil
	})

	if err != nil {
		return err
	}

	off, err := f.Seek(0, os.SEEK_END)
	if err != nil {
		return err
	}

	b := []byte{0, 0, 0, 0}
	binary.LittleEndian.PutUint32(b, uint32(did))

	// write at closest multiple of 4
	_, err = f.WriteAt(b, (off/4)*4)
	if err != nil {
		return err
	}
	return nil
}

type DocumentWithID interface {
	IndexableFields() map[string][]string
	DocumentID() int32
}

func (d *DirIndex) Index(docs ...DocumentWithID) error {
	var sb strings.Builder

	for _, doc := range docs {
		did := doc.DocumentID()

		fields := doc.IndexableFields()
		for field, value := range fields {
			field = termCleanup(field)
			if len(field) == 0 {
				continue
			}

			analyzer, ok := d.perField[field]
			if !ok {
				analyzer = DefaultAnalyzer
			}
			for _, v := range value {
				tokens := analyzer.AnalyzeIndex(v)
				for _, t := range tokens {
					t = termCleanup(t)
					if len(t) == 0 {
						continue
					}

					sb.WriteString(d.root)
					sb.WriteRune('/')
					sb.WriteString(field)
					sb.WriteRune('/')
					sb.WriteRune(rune(t[len(t)-1]))

					_ = os.MkdirAll(sb.String(), 0700)

					sb.WriteRune('/')
					sb.WriteString(t)

					err := d.add(sb.String(), did)
					if err != nil {
						return err
					}
					sb.Reset()
				}
			}
		}
	}
	return nil
}

func (d *DirIndex) Parse(input *spec.Query) (iq.Query, error) {
	return Parse(input, func(k, v string) iq.Query {
		terms := d.Terms(k, v)
		if len(terms) == 1 {
			return terms[0]
		}
		return iq.Or(terms...)
	})
}

func (d *DirIndex) Terms(field string, term string) []iq.Query {
	analyzer, ok := d.perField[field]
	if !ok {
		analyzer = DefaultAnalyzer
	}
	tokens := analyzer.AnalyzeSearch(term)
	queries := []iq.Query{}
	for _, t := range tokens {
		queries = append(queries, d.newTermQuery(field, t))
	}
	return queries
}

func (d *DirIndex) newTermQuery(field string, term string) iq.Query {
	field = termCleanup(field)
	term = termCleanup(term)
	if len(field) == 0 || len(term) == 0 {
		return iq.Term(d.TotalNumberOfDocs, fmt.Sprintf("broken(%s:%s)", field, term), []int32{})
	}
	fn := path.Join(d.root, field, string(term[len(term)-1]), term)
	data, err := ioutil.ReadFile(fn)
	if err != nil {
		return iq.Term(d.TotalNumberOfDocs, fn, []int32{})
	}
	postings := make([]int32, len(data)/4)
	for i := 0; i < len(postings); i++ {
		from := i * 4
		postings[i] = int32(binary.LittleEndian.Uint32(data[from : from+4]))
	}
	return iq.Term(d.TotalNumberOfDocs, fn, postings)
}

func (d *DirIndex) Close() {
	d.fdCache.Close()
}

func (d *DirIndex) Foreach(query iq.Query, cb func(int32, float32)) {
	for query.Next() != iq.NO_MORE {
		did := query.GetDocId()
		score := query.Score()

		cb(did, score)
	}
}