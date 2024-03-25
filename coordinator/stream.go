/*
Copyright 2022 Huawei Cloud Computing Technologies Co., Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package coordinator

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/fasttime"
	"github.com/openGemini/openGemini/engine/hybridqp"
	"github.com/openGemini/openGemini/lib/config"
	"github.com/openGemini/openGemini/lib/errno"
	"github.com/openGemini/openGemini/lib/logger"
	"github.com/openGemini/openGemini/lib/netstorage"
	streamLib "github.com/openGemini/openGemini/lib/stream"
	"github.com/openGemini/openGemini/lib/util/lifted/influx/influxql"
	meta2 "github.com/openGemini/openGemini/lib/util/lifted/influx/meta"
	"github.com/openGemini/openGemini/lib/util/lifted/influx/query"
	"github.com/openGemini/openGemini/lib/util/lifted/vm/protoparser/influx"
	"go.uber.org/zap"
)

type streamTask struct {
	info           *meta2.StreamInfo
	calls          []*streamLib.FieldCall
	tagDimKeys     []string
	fieldIndexKeys []string
}

func newStreamTask(info *meta2.StreamInfo, srcSchema, dstSchema map[string]int32) (*streamTask, error) {
	w := &streamTask{
		info: info,
	}
	var err error
	w.calls, err = BuildFieldCall(info, srcSchema, dstSchema)
	if err != nil {
		return nil, err
	}
	tagDimKeys, fieldIndexKeys := buildTagsFields(info, srcSchema)
	w.tagDimKeys = make([]string, len(tagDimKeys))
	w.fieldIndexKeys = make([]string, len(fieldIndexKeys))

	copy(w.tagDimKeys, tagDimKeys)
	copy(w.fieldIndexKeys, fieldIndexKeys)
	return w, nil
}

type TSDBStore interface {
	WriteRows(ctx *netstorage.WriteContext, nodeID uint64, pt uint32, database, rp string, timeout time.Duration) error
}

type Stream struct {
	TSDBStore TSDBStore

	MetaClient PWMetaClient
	logger     *logger.Logger
	timeout    time.Duration
	tasks      map[string]*streamTask
}

func NewStream(tsdbStore TSDBStore, metaClient PWMetaClient, logger *logger.Logger, timeout time.Duration) *Stream {
	return &Stream{
		TSDBStore:  tsdbStore,
		MetaClient: metaClient,
		logger:     logger,
		timeout:    timeout,
		tasks:      map[string]*streamTask{},
	}
}

var streamCtxPool sync.Pool

func GetStreamCtx() *streamCtx {
	v := streamCtxPool.Get()
	if v == nil {
		return &streamCtx{}
	}
	return v.(*streamCtx)
}

func PutStreamCtx(s *streamCtx) {
	s.reset()
	streamCtxPool.Put(s)
}

type streamCtx struct {
	minTime         int64
	db              *meta2.DatabaseInfo
	rp              *meta2.RetentionPolicyInfo
	ms              *meta2.MeasurementInfo
	shardKeyInfo    *meta2.ShardKeyInfo
	writeHelper     *writeHelper
	opt             *query.ProcessorOptions
	aliveShardIdxes []int
	dataCache       map[string]map[int64][]*float64
}

func (s *streamCtx) reset() {
	s.minTime = 0
	s.db = nil
	s.rp = nil
	s.ms = nil
	s.writeHelper = nil
	s.shardKeyInfo = nil
	s.opt = nil
	s.aliveShardIdxes = s.aliveShardIdxes[:0]
	s.dataCache = make(map[string]map[int64][]*float64)
}

func (s *streamCtx) checkDBRP(database, retentionPolicy string, w *Stream) (err error) {
	// check the validity of the database
	s.db, err = w.MetaClient.Database(database)
	if err != nil {
		return err
	}

	if retentionPolicy == "" {
		retentionPolicy = s.db.DefaultRetentionPolicy
	}

	// check the validity of the retention policy
	s.rp, err = s.db.GetRetentionPolicy(retentionPolicy)
	if err != nil {
		return err
	}

	if s.rp.Duration > 0 {
		s.minTime = int64(fasttime.UnixTimestamp()*1e9) - s.rp.Duration.Nanoseconds()
	}
	return
}

func (s *streamCtx) initVar(w *PointsWriter, si *meta2.StreamInfo) (err error) {
	// init the writerHelper, opt and measurementInfo
	if s.writeHelper == nil {
		s.writeHelper = newWriteHelper(w)
	}

	if s.opt == nil {
		s.opt = &query.ProcessorOptions{Interval: hybridqp.Interval{Duration: si.Interval}}
	}

	if s.dataCache == nil {
		s.dataCache = make(map[string]map[int64][]*float64)
	}

	if s.ms == nil {
		s.ms, err = s.writeHelper.createMeasurement(si.DesMst.Database, si.DesMst.RetentionPolicy, si.DesMst.Name)
		if err != nil {
			return err
		}
	}
	return
}

func (s *Stream) calculate(
	rows []*influx.Row, si *meta2.StreamInfo, pw *PointsWriter, iCtx *injestionCtx, idx int,
) error {
	ctx := GetStreamCtx()
	defer PutStreamCtx(ctx)

	task, ok := s.tasks[si.Name]
	if !ok {
		return fmt.Errorf("%s have no task", si.Name)
	}

	err := ctx.checkDBRP(si.DesMst.Database, si.DesMst.RetentionPolicy, s)
	if err != nil {
		return err
	}

	err = ctx.initVar(pw, si)
	if err != nil {
		return err
	}

	err = s.calculateWindow(rows, si, task, ctx)
	if err != nil {
		return err
	}

	err = s.mapRowsToShard(si, task, ctx, iCtx, idx)
	if err != nil {
		return err
	}
	return nil
}

func (s *Stream) calculateWindow(rows []*influx.Row, si *meta2.StreamInfo, task *streamTask, ctx *streamCtx) error {
	for _, r := range rows {
		groupKey := s.GenerateGroupKey(ctx, si.Dims, r)
		// get the end time of the window corresponding to this time,
		// and subtract 1 to avoid this time from expiring.
		_, et := ctx.opt.Window(r.Timestamp)
		et = et - 1
		v, ok := ctx.dataCache[groupKey]
		if !ok {
			ctx.dataCache[groupKey] = make(map[int64][]*float64)
			v = ctx.dataCache[groupKey]
			ctx.dataCache[groupKey][et] = make([]*float64, len(task.calls))
		} else if _, ok := v[et]; !ok {
			v[et] = make([]*float64, len(task.calls))
		}
		for i := range task.calls {
			id, ok := r.ColumnToIndex[task.calls[i].Name]
			if !ok {
				//miss field value
				continue
			}
			fv := r.Fields[id-r.Tags.Len()]
			if fv.Type == influx.Field_Type_String {
				// the computation of string type is not supported
				return fmt.Errorf("the %s string type is not supported for stream task %s", fv.Key, si.Name)
			}
			curVal := fv.NumValue
			if task.calls[i].Call == "count" {
				curVal = 1
			}
			if v[et][i] == nil {
				var t float64
				if task.calls[i].Call == "min" {
					t = math.MaxFloat64
				} else if task.calls[i].Call == "max" {
					t = -math.MaxFloat64
				}
				v[et][i] = &t
			}
			*v[et][i] = task.calls[i].SingleThreadFunc(*v[et][i], curVal)
		}
	}
	return nil
}

func (s *Stream) mapRowsToShard(
	si *meta2.StreamInfo, task *streamTask, ctx *streamCtx, iCtx *injestionCtx, idx int,
) error {
	wRows := iCtx.getPRowsPool()

	size := 0
	dimLen := len(task.tagDimKeys) + len(task.fieldIndexKeys)
	callLen := len(task.calls)
	srcStreamDstShardIdMap := iCtx.getSrcStreamDstShardIdMap()
	mstName := iCtx.streamMSTs[idx].Name
	oriLen, oriCap := len(*wRows), cap(*wRows)
	*wRows = (*wRows)[:oriCap]
	for i := oriLen; i < oriCap; i++ {
		(*wRows)[i] = &influx.Row{}
	}
	for k, tv := range ctx.dataCache {
		var groupValue []string
		if len(k) != 0 {
			groupValue = strings.Split(k, config.StreamGroupValueStrSeparator)
			if len(groupValue) != dimLen {
				errStr := fmt.Sprintf("group value is mssing for stream task %s, groupValue %v, tagDimKeys %v, fieldIndexKeys %v groupLen %v dimLen %v",
					si.Name, groupValue, task.tagDimKeys, task.fieldIndexKeys, len(groupValue), dimLen)
				return errors.New(errStr)
			}
		}
		for t, v := range tv {
			size++
			if len(*wRows) < size {
				*wRows = append(*wRows, &influx.Row{})
			}
			r := (*wRows)[size-1]
			r.Reset()

			// update the fields of the agg row
			if r.Fields == nil || cap(r.Fields) < callLen {
				r.Fields = make([]influx.Field, len(task.calls))
			}
			var fieldCount int
			r.Fields = r.Fields[:len(task.calls)]
			for i := range task.calls {
				if v[i] == nil {
					continue
				}
				r.Fields[i].Key = task.calls[i].Alias
				r.Fields[i].NumValue = *v[i]
				r.Fields[i].Type = task.calls[i].OutFieldType
				fieldCount++
			}
			if fieldCount == 0 {
				//no field, skip group
				continue
			}
			r.Fields = r.Fields[:fieldCount]

			if dimLen != 0 {
				// update the tags and columnToIndex of the agg row
				if r.Tags == nil || cap(r.Tags) < dimLen {
					r.Tags = make([]influx.Tag, dimLen)
				}
				r.Tags = r.Tags[:dimLen]
				if r.ColumnToIndex == nil {
					r.ColumnToIndex = make(map[string]int)
				}
				index := 0
				for i := range task.tagDimKeys {
					r.Tags[index].Key = task.tagDimKeys[i]
					r.Tags[index].Value = groupValue[i]
					r.ColumnToIndex[r.Tags[index].Key] = index
					index++
				}
			}

			// update the mst, timestamp and shardKey of the agg row
			r.Name = mstName
			r.Timestamp = t
			r.StreamOnly = true
			err, sh, pErr := s.updateShardGroupAndShardKey(si.DesMst.Database, si.DesMst.RetentionPolicy, r, ctx, si.Dims)
			if err != nil {
				return err
			}
			if pErr != nil {
				continue
			}
			r.StreamId = append(r.StreamId, si.ID)
			m, exist := srcStreamDstShardIdMap[sh.ID]
			if !exist {
				m = map[uint64]uint64{}
			}
			m[si.ID] = sh.ID
			srcStreamDstShardIdMap[sh.ID] = m
			iCtx.setShardRow(sh, r)
		}
	}
	*wRows = (*wRows)[:size]
	return nil
}

func (s *Stream) updateShardGroupAndShardKey(database, retentionPolicy string, r *influx.Row, ctx *streamCtx,
	dims []string) (err error, sh *meta2.ShardInfo, partialErr error) {
	var wh *writeHelper
	var di *meta2.DatabaseInfo
	var shardKeyInfo **meta2.ShardKeyInfo
	var mi *meta2.MeasurementInfo
	var aliveShardIdxes *[]int

	wh = ctx.writeHelper
	di = ctx.db
	shardKeyInfo = &ctx.shardKeyInfo
	mi = ctx.ms
	aliveShardIdxes = &ctx.aliveShardIdxes
	engineType := mi.EngineType
	sg, sameSg, err := wh.createShardGroup(database, retentionPolicy, time.Unix(0, r.Timestamp), engineType)
	if err != nil {
		return
	}

	if !sameSg {
		if len(di.ShardKey.ShardKey) > 0 {
			*shardKeyInfo = &di.ShardKey
		} else {
			*shardKeyInfo = mi.GetShardKey(sg.ID)
		}

		if *shardKeyInfo == nil {
			err = errno.NewError(errno.WriteNoShardKey)
			return
		}
	}

	if err = r.UnmarshalShardKeyByDimOrTag((*shardKeyInfo).ShardKey, dims); err != nil {
		if err != influx.ErrPointShouldHaveAllShardKey {
			return
		}
		partialErr = err
		err = nil
		return
	}

	if len(r.ShardKey) > MaxShardKey {
		partialErr = errno.NewError(errno.WritePointShardKeyTooLarge)
		s.logger.Error("write failed", zap.Error(partialErr))
		return
	}

	if !sameSg {
		*aliveShardIdxes = s.MetaClient.GetAliveShards(database, sg)
	}

	if (*shardKeyInfo).Type == influxql.RANGE {
		sh = sg.DestShard(bytesutil.ToUnsafeString(r.ShardKey))
	} else {
		if len((*shardKeyInfo).ShardKey) > 0 {
			r.ShardKey = r.ShardKey[len(r.Name)+1:]
		}
		sh = sg.ShardFor(meta2.HashID(r.ShardKey), *aliveShardIdxes)
	}
	if sh == nil {
		err = errno.NewError(errno.WritePointMap2Shard)
	}
	return
}

func (s *Stream) GenerateGroupKey(ctx *streamCtx, keys []string, value *influx.Row) string {
	if len(keys) == 0 {
		return ""
	}

	values := make([]string, len(keys))
	tags := value.Tags
	for i, k := range keys {
		idx := sort.Search(len(tags), func(j int) bool {
			return tags[j].Key >= k
		})

		if idx >= len(tags) {
			break
		}

		if tags[idx].Key == k {
			values[i] = tags[idx].Value
		}

		tags = tags[idx+1:]
	}

	return strings.Join(values, string(config.StreamGroupValueSeparator))
}

func BuildFieldCall(info *meta2.StreamInfo, srcSchema map[string]int32, destSchema map[string]int32) ([]*streamLib.FieldCall, error) {
	calls := make([]*streamLib.FieldCall, len(info.Calls))
	var err error
	for i, v := range info.Calls {
		calls[i], err = streamLib.NewFieldCall(srcSchema[v.Field], destSchema[v.Alias], v.Field, v.Alias, v.Call, false)
		if err != nil {
			return nil, err
		}
	}
	return calls, nil
}
