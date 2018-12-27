/*
 * Tencent is pleased to support the open source community by making 蓝鲸 available.
 * Copyright (C) 2017-2018 THL A29 Limited, a Tencent company. All rights reserved.
 * Licensed under the MIT License (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 * http://opensource.org/licenses/MIT
 * Unless required by applicable law or agreed to in writing, software distributed under
 * the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
 * either express or implied. See the License for the specific language governing permissions and
 * limitations under the License.
 */

package logics

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	com "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/errors"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/regions"
	cvm "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/cvm/v20170312"

	"configcenter/src/common"
	"configcenter/src/common/blog"
	"configcenter/src/common/mapstr"
	meta "configcenter/src/common/metadata"
	"configcenter/src/common/util"
	hutil "configcenter/src/scene_server/host_server/util"
)

var (
	taskInfoMap = make(map[int64]meta.TaskInfo, 0)
)

func (lgc *Logics) AddCloudTask(ctx context.Context, taskList *meta.CloudTaskList) error {
	// TaskName Uniqueness check
	resp, err := lgc.CoreAPI.HostController().Cloud().TaskNameCheck(ctx, lgc.header, taskList)
	if err != nil {
		return err
	}

	if resp.Data != 0.0 {
		blog.Errorf("add task failed, task name %s already exits.", taskList.TaskName)
		return lgc.ccErr.Error(1110038)
	}

	// Encode secretKey
	// taskList.SecretKey = base64.StdEncoding.EncodeToString([]byte(taskList.SecretKey))

	if _, err := lgc.CoreAPI.HostController().Cloud().AddCloudTask(context.Background(), lgc.header, taskList); err != nil {
		blog.Errorf("add cloud task failed, err: %v", err)
		return err
	}

	return nil
}

func (lgc *Logics) InitFunc() {
	lgc.TimerTriggerCheckStatus()
}

func (lgc *Logics) TimerTriggerCheckStatus() {
	timer := time.NewTicker(5 * time.Minute)
	for range timer.C {
		go lgc.SyncTaskDBManager(context.Background())
		go lgc.SyncTaskRedisManager(context.Background())
	}
}

func (lgc *Logics) SyncTaskDBManager(ctx context.Context) error {
	var mutex = &sync.Mutex{}
	taskInfoRedisArr := make([]interface{}, 0)

	opt := make(map[string]interface{})
	resp, err := lgc.CoreAPI.HostController().Cloud().SearchCloudTask(ctx, lgc.header, opt)
	if err != nil {
		blog.Errorf("get cloud sync task instance failed, %v", err)
		return lgc.ccErr.Error(1110036)
	}

	for _, taskInfo := range resp.Info {
		taskID := taskInfo.TaskID
		if _, ok := taskInfoMap[taskID]; ok {
			continue
		}
		nextTrigger := lgc.NextTrigger(ctx, taskInfo.PeriodType, taskInfo.Period)
		taskInfoItem := meta.TaskInfo{
			Method:      taskInfo.PeriodType,
			NextTrigger: nextTrigger,
			Args:        taskInfo,
		}

		mutex.Lock()
		taskInfoMap[taskID] = taskInfoItem
		mutex.Unlock()

		lgc.CloudSyncSwitch(ctx, taskInfoItem)
		info := meta.CloudSyncRedisStart{TaskID: taskInfo.TaskID, Admin: taskInfo.AccountAdmin, StartTime: time.Now()}
		startedTaskInfo, err := json.Marshal(info)
		if err != nil {
			blog.Errorf("add redis failed taskID: %v, accountAdmin: %v", taskInfo.TaskID, taskInfo.AccountAdmin)
			continue
		}
		taskInfoRedisArr = append(taskInfoRedisArr, startedTaskInfo)
	}

	if err := lgc.cache.SAdd(common.RedisCloudSyncInstanceStarted, taskInfoRedisArr...).Err(); err != nil {
		blog.Errorf("add cloud task item to redis fail, err: %v", err)
	}

	return err
}

func (lgc *Logics) SyncTaskRedisManager(ctx context.Context) {
	for {
		stop, err := lgc.cache.SPop(common.RedisCloudSyncInstancePendingStop).Result()
		if err != nil {
			blog.Errorf("get stop item from redis fail, err: v%", err.Error())
		}

		item := &meta.CloudSyncRedisStop{}
		err = json.Unmarshal([]byte(stop), item)
		if nil != err {
			blog.Warnf("get task stop item from redis fail, error:%s", err.Error())
			continue
		}

		ownerID := util.GetOwnerID(lgc.header)
		if ownerID == item.OwnerID {
			taskInfoItem, ok := taskInfoMap[item.TaskID]
			if !ok {
				return
			}
			taskInfoItem.ManagerChn <- true
			delete(taskInfoMap, item.TaskID)
			lgc.CloudSyncSwitch(context.Background(), taskInfoItem)
		}

		lgc.cache.SDiffStore(common.RedisCloudSyncInstanceStarted, common.RedisCloudSyncInstanceStarted, common.RedisCloudSyncInstancePendingStop)
	}
}

func (lgc *Logics) CloudSyncSwitch(ctx context.Context, taskInfoItem meta.TaskInfo) {
	for {
		ticker := time.NewTicker(time.Duration(taskInfoItem.NextTrigger) * time.Minute)
		select {
		case <-ticker.C:
			lgc.ExecSync(ctx, taskInfoItem.Args)
			switch taskInfoItem.Method {
			case "day":
				taskInfoItem.NextTrigger = 1440
			case "hour":
				taskInfoItem.NextTrigger = 60
			case "minute":
				taskInfoItem.NextTrigger = 5
			}
		case <-taskInfoItem.ManagerChn:
			blog.Info("stop cloud sync")
			return
		}
	}
}

func (lgc *Logics) ExecSync(ctx context.Context, taskInfo meta.CloudTaskInfo) error {
	cloudHistory := new(meta.CloudHistory)
	cloudHistory.ObjID = taskInfo.ObjID
	cloudHistory.TaskID = taskInfo.TaskID
	startTime := time.Now().Unix()

	defer lgc.CloudSyncHistory(ctx, taskInfo.TaskID, startTime, cloudHistory)

	var errOrigin error
	defer func() {
		if errOrigin != nil {
			cloudHistory.Status = "fail"
		}
	}()

	// obtain the hosts from cc_HostBase
	body := new(meta.HostCommonSearch)
	host, err := lgc.SearchHost(ctx, body, false)
	if err != nil {
		blog.Errorf("search host failed, err: %v", err)
		errOrigin = err
		return err
	}

	existHostList := make([]string, 0)
	for i := 0; i < host.Count; i++ {
		hostInfo, err := mapstr.NewFromInterface(host.Info[i]["host"])
		if err != nil {
			blog.Errorf("get hostInfo failed with err: %v", err)
			errOrigin = err
			return err
		}

		ip, errH := hostInfo.String(common.BKHostInnerIPField)
		if errH != nil {
			blog.Errorf("get hostIp failed with err: %v")
			errOrigin = errH
			return errH
		}

		existHostList = append(existHostList, ip)
	}

	/*
		// obtain hosts from TencentCloud needs secretID and secretKey
		secretID, errS := taskList.String("bk_secret_id")
		if errS != nil {
			blog.Errorf("mapstr convert to string failed.")
			errOrigin = errS
			return errS
		}
		secretKeyEncrypted, errKey := taskList.String("bk_secret_key")
		if errKey != nil {
			blog.Errorf("mapstr convert to string failed.")
			errOrigin = errKey
			return errKey
		}

		decodeBytes, errDecode := base64.StdEncoding.DecodeString(taskInfo.SecretKey)
		if errDecode != nil {
			blog.Errorf("Base64 decode secretKey failed.")
			errOrigin = errDecode
			return errDecode
		}
		secretKey := string(decodeBytes)

	*/

	// ObtainCloudHosts obtain cloud hosts
	cloudHostInfo, err := lgc.ObtainCloudHosts(taskInfo.SecretID, taskInfo.SecretKey)
	if err != nil {
		blog.Errorf("obtain cloud hosts failed with err: %v", err)
		errOrigin = err
		return err
	}

	// pick out the new add cloud hosts
	newAddHost := make([]string, 0)
	newCloudHost := make([]mapstr.MapStr, 0)
	for _, hostInfo := range cloudHostInfo {
		newHostInnerip, ok := hostInfo[common.BKHostInnerIPField].(string)
		if !ok {
			blog.Errorf("interface convert to string failed")
		}
		if !util.InStrArr(existHostList, newHostInnerip) {
			newAddHost = append(newAddHost, newHostInnerip)
			newCloudHost = append(newCloudHost, hostInfo)
		}
	}

	// pick out the hosts that has changed attributes
	cloudHostAttr := make([]mapstr.MapStr, 0)
	for _, hostInfo := range cloudHostInfo {
		newHostInnerip, ok := hostInfo[common.BKHostInnerIPField].(string)
		if !ok {
			blog.Errorf("interface convert to string failed, err: %v", err)
			continue
		}
		newHostOuterip, ok := hostInfo[common.BKHostOuterIPField].(string)
		if !ok {
			blog.Errorf("interface convert to string failed, err: %v", err)
			continue
		}
		newHostOsname, ok := hostInfo[common.BKOSNameField].(string)
		if !ok {
			blog.Errorf("interface convert to string failed, err: %v", err)
			continue
		}

		for i := 0; i < host.Count; i++ {
			existHostInfo, err := mapstr.NewFromInterface(host.Info[i]["host"])
			if err != nil {
				blog.Errorf("get hostInfo failed with err: %v", err)
				errOrigin = err
				return err
			}

			existHostIp, ok := existHostInfo.String(common.BKHostInnerIPField)
			if ok != nil {
				blog.Errorf("get hostIp failed with err: %v", ok)
				errOrigin = ok
				break
			}
			existHostOsname, osOk := existHostInfo.String(common.BKOSNameField)
			if osOk != nil {
				blog.Errorf("get os name failed with err: %v", ok)
				errOrigin = osOk
				break
			}

			existHostOuterip, ipOk := existHostInfo.String(common.BKHostOuterIPField)
			if ipOk != nil {
				blog.Errorf("get outerip failed with")
				errOrigin = ipOk
				break
			}

			existHostID, idOk := existHostInfo.String(common.BKHostIDField)
			if idOk != nil {
				blog.Errorf("get hostID failed")
				errOrigin = idOk
				break
			}

			if existHostIp == newHostInnerip {
				if existHostOsname != newHostOsname || existHostOuterip != newHostOuterip {
					hostInfo[common.BKHostIDField] = existHostID
					cloudHostAttr = append(cloudHostAttr, hostInfo)
				}
			}
		}
	}

	cloudHistory.NewAdd = len(newAddHost)
	cloudHistory.AttrChanged = len(cloudHostAttr)

	attrConfirm := taskInfo.AttrConfirm
	resourceConfirm := taskInfo.ResourceConfirm

	if !resourceConfirm && !attrConfirm {
		if len(newCloudHost) > 0 {
			err := lgc.AddCloudHosts(ctx, newCloudHost)
			if err != nil {
				blog.Errorf("add cloud hosts failed, err: %v", err)
				errOrigin = err
				return err
			}
		}
		if len(cloudHostAttr) > 0 {
			err := lgc.UpdateCloudHosts(ctx, cloudHostAttr)
			if err != nil {
				blog.Errorf("update cloud hosts failed, err: %v", err)
				errOrigin = err
				return err
			}
		}
	}

	if resourceConfirm {
		newAddNum, err := lgc.NewAddConfirm(ctx, taskInfo, newCloudHost)
		cloudHistory.NewAdd = newAddNum
		if err != nil {
			blog.Errorf("newly add cloud resource confirm failed, err: %v", err)
			errOrigin = err
			return err
		}
	}

	if attrConfirm && len(cloudHostAttr) > 0 {
		blog.Debug("attr chang")
		for _, host := range cloudHostAttr {
			resourceConfirm := mapstr.MapStr{}
			resourceConfirm["bk_obj_id"] = taskInfo.ObjID
			innerIp, errIp := host.String(common.BKHostInnerIPField)
			if errIp != nil {
				blog.Debug("mapstr.Map convert to string failed.")
				errOrigin = errIp
				return errIp
			}
			outerIp, errOut := host.String(common.BKHostOuterIPField)
			if errOut != nil {
				blog.Error("mapstr.Map convert to string failed")
				errOrigin = errOut
				return errOut
			}
			osName, err := host.String(common.BKOSNameField)
			if err != nil {
				blog.Error("mapstr.Map convert to string failed")
				errOrigin = err
				return err
			}

			resourceConfirm[common.BKHostInnerIPField] = innerIp
			resourceConfirm[common.BKHostOuterIPField] = outerIp
			resourceConfirm[common.BKOSNameField] = osName
			resourceConfirm["bk_source_type"] = "cloud_sync"
			resourceConfirm["bk_task_id"] = taskInfo.TaskID
			resourceConfirm["bk_attr_confirm"] = attrConfirm
			resourceConfirm["bk_confirm"] = false
			resourceConfirm["bk_task_name"] = taskInfo.TaskName
			resourceConfirm["bk_account_type"] = taskInfo.AccountType
			resourceConfirm["bk_account_admin"] = taskInfo.AccountAdmin
			resourceConfirm["bk_resource_type"] = "change"

			if _, err := lgc.CoreAPI.HostController().Cloud().ResourceConfirm(ctx, lgc.header, resourceConfirm); err != nil {
				blog.Errorf("add resource confirm failed with err: %v", err)
				errOrigin = err
				return err
			}
		}
		return nil
	}

	cloudHistory.Status = "success"
	blog.V(3).Info("finish sync")
	return nil
}

func (lgc *Logics) AddCloudHosts(ctx context.Context, newCloudHost []mapstr.MapStr) error {
	hostList := new(meta.HostList)
	hostInfoMap := make(map[int64]map[string]interface{}, 0)
	appID := hostList.ApplicationID

	if appID == 0 {
		// get default app id
		var err error
		appID, err = lgc.GetDefaultAppIDWithSupplier(ctx)
		if err != nil {
			blog.Errorf("add host, but get default appid failed, err: %v", err)
			return err
		}
	}

	cond := hutil.NewOperation().WithModuleName(common.DefaultResModuleName).WithAppID(appID).Data()
	cond[common.BKDefaultField] = common.DefaultResModuleFlag
	moduleID, err := lgc.GetResoulePoolModuleID(ctx, cond)
	if err != nil {
		blog.Errorf("add host, but get module id failed, err: %s", err.Error())
		return err
	}

	blog.V(3).Info("resource confirm add new hosts")
	for index, hostInfo := range newCloudHost {
		if _, ok := hostInfoMap[int64(index)]; !ok {
			hostInfoMap[int64(index)] = make(map[string]interface{}, 0)
		}

		hostInfoMap[int64(index)][common.BKHostInnerIPField] = hostInfo[common.BKHostInnerIPField]
		hostInfoMap[int64(index)][common.BKHostOuterIPField] = hostInfo[common.BKHostOuterIPField]
		hostInfoMap[int64(index)][common.BKOSNameField] = hostInfo[common.BKOSNameField]
		hostInfoMap[int64(index)]["import_from"] = "3"
		hostInfoMap[int64(index)]["bk_cloud_id"] = 1
	}

	succ, updateErrRow, errRow, ok := lgc.AddHost(ctx, appID, []int64{moduleID}, util.GetOwnerID(lgc.header), hostInfoMap, hostList.InputType)
	if ok != nil {
		blog.Errorf("add host failed, succ: %v, update: %v, err: %v, %v", succ, updateErrRow, ok, errRow)
		return ok
	}

	return nil
}

func (lgc *Logics) UpdateCloudHosts(ctx context.Context, cloudHostAttr []mapstr.MapStr) error {
	for _, hostInfo := range cloudHostAttr {
		hostID, err := hostInfo.Int64(common.BKHostIDField)
		if err != nil {
			blog.Errorf("hostID convert to string failed")
			return err
		}

		delete(hostInfo, common.BKHostIDField)
		delete(hostInfo, "bk_confirm")
		delete(hostInfo, "bk_attr_confirm")
		opt := mapstr.MapStr{"condition": mapstr.MapStr{common.BKHostIDField: hostID}, "data": hostInfo}

		blog.V(3).Info("opt: %v", opt)
		result, err := lgc.CoreAPI.ObjectController().Instance().UpdateObject(ctx, common.BKInnerObjIDHost, lgc.header, opt)
		if err != nil || (err == nil && !result.Result) {
			blog.Errorf("update host batch failed, ids[%v], err: %v, %v", hostID, err, result.ErrMsg)
			return err
		}
	}
	return nil
}

func (lgc *Logics) NewAddConfirm(ctx context.Context, taskInfo meta.CloudTaskInfo, newCloudHost []mapstr.MapStr) (int, error) {
	// Check whether the host is already exist in resource confirm.
	opt := make(map[string]interface{})
	confirmHosts, errS := lgc.CoreAPI.HostController().Cloud().SearchConfirm(ctx, lgc.header, opt)
	if errS != nil {
		blog.Errorf("get confirm info failed with err: %v", errS)
		return 0, errS
	}

	confirmIpList := make([]string, 0)
	if confirmHosts.Count > 0 {
		for _, confirmInfo := range confirmHosts.Info {
			ip, ok := confirmInfo[common.BKHostInnerIPField].(string)
			if !ok {
				break
			}
			confirmIpList = append(confirmIpList, ip)
		}
	}

	newHostIp := make([]string, 0)
	for _, host := range newCloudHost {
		innerIp, errIp := host.String(common.BKHostInnerIPField)
		if errIp != nil {
			blog.Debug("mapstr.Map convert to string failed.")
			return 0, errIp
		}
		if !util.InStrArr(confirmIpList, innerIp) {
			newHostIp = append(newHostIp, innerIp)
		}
	}

	// newly added cloud hosts confirm
	if len(newHostIp) > 0 {
		for _, host := range newCloudHost {
			innerIp, errIp := host.String(common.BKHostInnerIPField)
			if errIp != nil {
				blog.Error("mapstr.Map convert to string failed")
				return 0, errIp
			}
			outerIp, errOut := host.String(common.BKHostOuterIPField)
			if errOut != nil {
				blog.Error("mapstr.Map convert to string failed")
				return 0, errOut
			}
			osName, errOs := host.String(common.BKOSNameField)
			if errOs != nil {
				blog.Error("mapstr.Map convert to string failed")
				return 0, errOs
			}
			resourceConfirm := mapstr.MapStr{}
			resourceConfirm["bk_obj_id"] = taskInfo.ObjID
			resourceConfirm[common.BKHostInnerIPField] = innerIp
			resourceConfirm["bk_source_type"] = "云同步"
			resourceConfirm["bk_task_id"] = taskInfo.TaskID
			resourceConfirm[common.BKOSNameField] = osName
			resourceConfirm[common.BKHostOuterIPField] = outerIp
			resourceConfirm["bk_confirm"] = true
			resourceConfirm["bk_attr_confirm"] = false
			resourceConfirm["bk_task_name"] = taskInfo.TaskName
			resourceConfirm["bk_account_type"] = taskInfo.AccountType
			resourceConfirm["bk_account_admin"] = taskInfo.AccountAdmin
			resourceConfirm["bk_resource_type"] = "new_add"

			_, err := lgc.CoreAPI.HostController().Cloud().ResourceConfirm(ctx, lgc.header, resourceConfirm)
			if err != nil {
				blog.Errorf("add resource confirm failed with err: %v", err)
				return 0, err
			}
		}
	}
	num := len(newHostIp)
	return num, nil
}

func (lgc *Logics) NextTrigger(ctx context.Context, periodType string, period string) int64 {
	timeLayout := "2006-01-02 15:04:05" // transfer model
	toBeCharge := period
	var unixSubtract int64
	nowStr := time.Unix(time.Now().Unix(), 0).Format(timeLayout)

	blog.Debug("periodType: %v", periodType)
	blog.Debug("period: %v", period)
	if periodType == "day" {
		intHour, _ := strconv.Atoi(toBeCharge[:2])
		intMinute, _ := strconv.Atoi(toBeCharge[3:])
		if intHour > time.Now().Hour() {
			toBeCharge = fmt.Sprintf("%s%s%s", nowStr[:11], toBeCharge, ":00")
		}
		if intHour < time.Now().Hour() {
			toBeCharge = fmt.Sprintf("%s%d %s%s", nowStr[:8], time.Now().Day()+1, toBeCharge, ":00")
		}
		if intHour == time.Now().Hour() && intMinute > time.Now().Minute() {
			toBeCharge = fmt.Sprintf("%s%s%s", nowStr[:11], toBeCharge, ":00")
		}
		if intHour == time.Now().Hour() && intMinute <= time.Now().Minute() {
			toBeCharge = fmt.Sprintf("%s%d %s%s", nowStr[:8], time.Now().Day()+1, toBeCharge, ":00")
		}

		loc, _ := time.LoadLocation("Local")
		theTime, _ := time.ParseInLocation(timeLayout, toBeCharge, loc)
		sr := theTime.Unix()
		unixSubtract = sr - time.Now().Unix()
	}

	if periodType == "hour" {
		intToBeCharge, err := strconv.Atoi(toBeCharge)
		if err != nil {
			blog.Errorf("period transfer to int failed with err: %v", err)
			return 0
		}

		if intToBeCharge >= 10 && intToBeCharge > time.Now().Minute() {
			toBeCharge = fmt.Sprintf("%s%d:%s:%s", nowStr[:11], time.Now().Hour(), toBeCharge, "00")
		}
		if intToBeCharge >= 10 && intToBeCharge < time.Now().Minute() {
			toBeCharge = fmt.Sprintf("%s%d:%s:%s", nowStr[:11], time.Now().Hour()+1, toBeCharge, "00")
		}
		if intToBeCharge < 10 && intToBeCharge > time.Now().Minute() {
			toBeCharge = fmt.Sprintf("%s%d:0%s:%s", nowStr[:11], time.Now().Hour(), toBeCharge, "00")
		}
		if intToBeCharge < 10 && intToBeCharge < time.Now().Minute() {
			toBeCharge = fmt.Sprintf("%s%d:0%s:%s", nowStr[:11], time.Now().Hour()+1, toBeCharge, "00")
		}

		loc, _ := time.LoadLocation("Local")
		theTime, _ := time.ParseInLocation(timeLayout, toBeCharge, loc)
		sr := theTime.Unix()
		unixSubtract = sr - time.Now().Unix()
	}

	if periodType == "minute" {
		unixSubtract = 300
	}

	minuteNextTrigger := unixSubtract / 60
	return minuteNextTrigger
}

func (lgc *Logics) FrontEndSyncSwitch(ctx context.Context, opt map[string]interface{}) error {
	response, err := lgc.CoreAPI.HostController().Cloud().SearchCloudTask(ctx, lgc.header, opt)
	if err != nil {
		blog.Errorf("search cloud task instance failed, err: %v", err)
		return lgc.ccErr.Error(1110036)
	}

	taskInfoRedisArr := make([]interface{}, 0)
	taskRedisPendingStop := make([]interface{}, 0)
	var mutex = &sync.Mutex{}

	if response.Count > 0 {
		taskInfo := response.Info[0]
		status := taskInfo.Status
		taskID := taskInfo.TaskID
		if _, ok := taskInfoMap[taskID]; ok {
			return nil
		}

		if status {
			nextTrigger := lgc.NextTrigger(ctx, taskInfo.PeriodType, taskInfo.Period)
			taskInfoItem := meta.TaskInfo{
				Method:      taskInfo.PeriodType,
				NextTrigger: nextTrigger,
				Args:        taskInfo,
			}

			mutex.Lock()
			taskInfoMap[taskID] = taskInfoItem
			mutex.Unlock()

			ownerID := util.GetOwnerID(lgc.header)
			info := meta.CloudSyncRedisStart{TaskID: taskInfo.TaskID, Admin: taskInfo.AccountAdmin, StartTime: time.Now(), OwnerID: ownerID}
			startedTaskInfo, err := json.Marshal(info)
			if err != nil {
				blog.Errorf("add redis failed taskID: %v, accountAdmin: %v", taskInfo.TaskID, taskInfo.AccountAdmin)
			}
			taskInfoRedisArr = append(taskInfoRedisArr, startedTaskInfo)
			if err := lgc.cache.SAdd(common.RedisCloudSyncInstanceStarted, taskInfoRedisArr...).Err(); err != nil {
				blog.Errorf("add cloud task redis item fail, err: %v", err)
			}

			lgc.CloudSyncSwitch(ctx, taskInfoItem)
		} else {
			ownerID := util.GetOwnerID(lgc.header)
			info := meta.CloudSyncRedisStop{TaskID: taskInfo.TaskID, OwnerID: ownerID}
			stopTaskInfo, err := json.Marshal(info)
			if err != nil {
				blog.Errorf("stop task add redis failed taskID: %v, accountAdmin: %v", taskInfo.TaskID, taskInfo.AccountAdmin)
			}
			taskRedisPendingStop = append(taskRedisPendingStop, stopTaskInfo)
			if err := lgc.cache.SAdd(common.RedisCloudSyncInstancePendingStop, taskRedisPendingStop...).Err(); err != nil {
				blog.Errorf("add cloud task redis item fail, err: %v", err)
			}
		}
	}

	return nil
}

func (lgc *Logics) CloudSyncHistory(ctx context.Context, taskID int64, startTime int64, cloudHistory *meta.CloudHistory) error {
	finishTime := time.Now().Unix()
	timeConsumed := finishTime - startTime
	if timeConsumed > 60 {
		minute := timeConsumed / 60
		seconds := timeConsumed % 60
		cloudHistory.TimeConsume = fmt.Sprintf("%dmin%ds", minute, seconds)
	} else {
		cloudHistory.TimeConsume = fmt.Sprintf("%ds", timeConsumed)
	}

	timeLayout := "2006-01-02 15:04:05" // transfer model
	startTimeStr := time.Unix(startTime, 0).Format(timeLayout)
	cloudHistory.StartTime = startTimeStr

	blog.V(3).Info(cloudHistory.TimeConsume)

	updateData := mapstr.MapStr{}
	updateTime := time.Now()
	updateData["bk_last_sync_time"] = updateTime
	updateData["bk_task_id"] = taskID
	updateData["bk_sync_status"] = cloudHistory.Status
	updateData["new_add"] = cloudHistory.NewAdd
	updateData["attr_changed"] = cloudHistory.AttrChanged

	if _, err := lgc.CoreAPI.HostController().Cloud().UpdateCloudTask(ctx, lgc.header, updateData); err != nil {
		blog.Errorf("update task failed with decode body err: %v", err)
		return err
	}

	if _, err := lgc.CoreAPI.HostController().Cloud().AddSyncHistory(ctx, lgc.header, cloudHistory); err != nil {
		blog.Errorf("add cloud history table failed, err: %v", err)
		return err
	}

	return nil
}

func (lgc *Logics) ObtainCloudHosts(secretID string, secretKey string) ([]map[string]interface{}, error) {
	credential := com.NewCredential(
		secretID,
		secretKey,
	)

	cpf := profile.NewClientProfile()
	cpf.HttpProfile.ReqMethod = "GET"
	cpf.HttpProfile.ReqTimeout = 10
	cpf.HttpProfile.Endpoint = "cvm.tencentcloudapi.com"
	cpf.SignMethod = "HmacSHA1"

	ClientRegion, _ := cvm.NewClient(credential, regions.Guangzhou, cpf)
	regionRequest := cvm.NewDescribeRegionsRequest()
	Response, err := ClientRegion.DescribeRegions(regionRequest)

	if err != nil {
		return nil, err
	}

	data := Response.ToJsonString()
	regionResponse := new(meta.RegionResponse)
	if err := json.Unmarshal([]byte(data), regionResponse); err != nil {
		blog.Errorf("json unmarsha1 error :%v\n", err)
		return nil, err
	}

	cloudHostInfo := make([]map[string]interface{}, 0)
	for _, region := range regionResponse.Response.Data {
		var inneripList string
		var outeripList string
		var osName string
		regionHosts := make(map[string]interface{})

		client, _ := cvm.NewClient(credential, region.Region, cpf)
		instRequest := cvm.NewDescribeInstancesRequest()
		response, err := client.DescribeInstances(instRequest)

		if _, ok := err.(*errors.TencentCloudSDKError); ok {
			fmt.Printf("An API error has returned: %s", err)
			return nil, err
		}
		if err != nil {
			blog.Error("obtain cloud hosts failed")
			return nil, err
		}

		data := response.ToJsonString()
		Hosts := meta.HostResponse{}
		if err := json.Unmarshal([]byte(data), &Hosts); err != nil {
			fmt.Printf("json unmarsha1 error :%v\n", err)
		}

		instSet := Hosts.HostResponse.InstanceSet
		for _, obj := range instSet {
			osName = obj.OsName
			if len(obj.PrivateIpAddresses) > 0 {
				inneripList = obj.PrivateIpAddresses[0]
			}
		}

		for _, obj := range instSet {
			if len(obj.PublicIpAddresses) > 0 {
				outeripList = obj.PublicIpAddresses[0]
			}
		}

		if len(instSet) > 0 {
			regionHosts["bk_cloud_region"] = region.Region
			regionHosts["bk_host_innerip"] = inneripList
			regionHosts["bk_host_outerip"] = outeripList
			regionHosts["bk_os_name"] = osName
			cloudHostInfo = append(cloudHostInfo, regionHosts)
		}
	}
	return cloudHostInfo, nil
}
