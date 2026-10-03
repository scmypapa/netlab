import assert from 'node:assert/strict'
import {randomUUID} from 'node:crypto'
import {execFile} from 'node:child_process'
import {promisify} from 'node:util'
import {readFile,writeFile} from 'node:fs/promises'
import {delay} from './guest-ssh.mjs'

const execute=promisify(execFile),run=randomUUID(),base='http://127.0.0.1:8090',second='http://127.0.0.1:8091'
const workers=new Map(JSON.parse(await readFile('D:/.cache/netlab/artifacts/multi-node-workers.json','utf8')).map(worker=>[worker.nodeId,worker]))
const report={startedAt:new Date().toISOString(),steps:[],cleanupErrors:[]},pools=[]
const source=`/var/lib/netlab-dev/test-tmp/recovery-${run}`
let cookie,environment,point,vmTemplate,containerTemplate,before,baseline,stoppedNode,readOnlyCapture
const quote=value=>`'${String(value).replaceAll("'","'\"'\"'")}'`
async function node(id,...args){const w=workers.get(id),command=w.host ? ['ssh','-i',w.keyPath,'-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o',`UserKnownHostsFile=${w.keyPath}.hosts`,`root@${w.host}`,args.map(quote).join(' ')] : args;try{return (await execute('wsl.exe',['-d','Ubuntu','-u','root','--exec',...command],{timeout:120000,maxBuffer:2**20})).stdout}catch(error){error.message+=`\n${error.stdout ?? ''}`;throw error}}
async function api(path,method='GET',body,status=200){const response=await fetch(`${method==='DELETE' ? second : base}/api/v1${path}`,{method,headers:{'Content-Type':'application/json',...(cookie ? {Cookie:cookie} : {})},body:body===undefined ? undefined : JSON.stringify(body)});if(response.headers.get('set-cookie'))cookie=response.headers.get('set-cookie').split(';')[0];const text=await response.text();assert.equal(response.status,status,`${method} ${path}: ${text}`);return text ? JSON.parse(text) : undefined}
async function operation(id,expected='succeeded'){for(const deadline=Date.now()+120000;Date.now()<deadline;){const op=await api(`/operations/${id}`);if(['succeeded','failed','partially_applied'].includes(op.state)){assert.equal(op.state,expected,`${op.phase}: ${op.error}`);return op}await delay(150)}throw new Error(`operation timeout: ${id}`)}
async function connected(ids,after=-Infinity){for(const deadline=Date.now()+30000;Date.now()<deadline;){const nodes=await api('/nodes');if(ids.every(id=>nodes.some(n=>n.id===id && n.state==='ready' && Date.parse(n.observedAt)>after)))return;await delay(200)}throw new Error('test workers did not reconnect')}
async function action(kind,assetId){const op=await api(`/environments/${environment.id}${assetId ? `/assets/${assetId}` : ''}/actions`,'POST',{action:kind},202);return operation(op.id)}
async function step(name,fn){const result={name,passed:false},start=performance.now();try{await fn();result.passed=true}catch(error){result.error=error.message;throw error}finally{result.durationMs=Math.round(performance.now()-start);report.steps.push(result);console.log(JSON.stringify(result))}}
function dir(actual){return `${pools.find(p=>p.nodeId===actual.nodeId).storage.path}/recovery-points/${point.id}/${actual.assetId}`}
async function deletePoint(){if(!point)return;const found=(await api(`/environments/${environment.id}/recovery-points`)).find(p=>p.id===point.id);if(!found)return;const op=found.state==='deleting' ? await api(`/operations/${found.operationId}/retry`,'POST',undefined,202) : await api(`/environments/${environment.id}/recovery-points/${point.id}`,'DELETE',undefined,202);await operation(op.id)}
try {
  await api('/sessions/login','POST',JSON.parse(await readFile('data/dev-login.json','utf8')))
  await connected([...workers.keys()])
  baseline=(await api('/nodes')).map(n=>({id:n.id,reserved:n.reserved}))
  await step('双节点真实 containerd 与 UEFI、Secure Boot、TPM KVM 混合环境',async()=>{
    const primary=[...workers.keys()].find(id=>!workers.get(id).host)
    for(const id of workers.keys()) {await node(id,'mkdir','-p',source);await node(id,'qemu-img','create','-f','qcow2',`${source}/base.qcow2`,'1G');pools.push(await api('/storage-pools','POST',{nodeId:id,name:`Recovery ${run}`,directory:source},201))}
    containerTemplate=(await api('/templates?kind=container')).find(t=>t.state==='ready' && t.name.includes('container'))
    assert(containerTemplate,'missing existing nginx container template')
    vmTemplate=await api('/templates','POST',{name:`Recovery UEFI ${run}`,kind:'vm',os:'Linux',version:1,source:`${source}/base.qcow2`,format:'qcow2',resources:{cpu:1,memoryMiB:128,diskGiB:1},hardware:{machine:'q35',firmware:'uefi',secureBoot:true,tpm:true,diskBus:'virtio',nicModel:'virtio'}},201)
    await operation(vmTemplate.operationId)
    vmTemplate=(await api(`/templates?ids=${vmTemplate.id}`))[0]
    const network={id:randomUUID(),name:'Recovery LAN',cidr:'192.168.84.0/24'}
    const assets=pools.flatMap(pool=>[containerTemplate,vmTemplate].map(t=>({id:randomUUID(),name:`${t.kind}-${pool.nodeId.slice(0,4)}`,templateId:t.id,storagePoolId:pool.id,resources:t.resources,interfaces:[{id:randomUUID(),networkId:network.id,mac:'',address:'',primary:true}],volumes:[{id:'data',mountPath:'/data',sizeGiB:1}]})))
    environment=await api('/environments','POST',{name:`Recovery ${run}`,spec:{networks:[network],assets},run:true},201);report.environmentId=environment.id;await operation(environment.operationId)
    await action('force-stop')
    before=await api(`/environments/${environment.id}/state`)
    for(const actual of before.assets){const root=pools.find(p=>p.nodeId===actual.nodeId).storage.path;if(assets.find(a=>a.id===actual.assetId).templateId===vmTemplate.id){for(const path of [`${root}/environments/${environment.id}/instances/${actual.instanceId}/disk-0.qcow2`,`${root}/environments/${environment.id}/volumes/${actual.assetId}/data.qcow2`])await node(actual.nodeId,'qemu-io','-f','qcow2','-c','write -P 0x59 0 4096',path)}else{await action('start',actual.assetId);await node(actual.nodeId,'ctr','-n','netlab','tasks','exec','--exec-id',randomUUID(),actual.instanceId,'/bin/sh','-c','printf original > /root/recovery-marker; rm /usr/share/nginx/html/50x.html; printf original > /data/marker; chmod 600 /data/marker; ln -s marker /data/marker-link')}}
    before=await api(`/environments/${environment.id}/state`)
    const suspended=before.assets.find(a=>a.nodeId!==primary && assets.find(x=>x.id===a.assetId).templateId===containerTemplate.id)
    await action('suspend',suspended.assetId)
    before=await api(`/environments/${environment.id}/state`)
    report.originalStates=before.assets.map(a=>({assetId:a.assetId,instanceId:a.instanceId,nodeId:a.nodeId,state:a.state}))
  })
  await step('捕获写入真实失败、恢复原状态、解除故障后原任务重试',async()=>{
    const pool=pools[1],path=`${pool.storage.path}/recovery-points`
    await node(pool.nodeId,'mkdir','-p',path)
    await node(pool.nodeId,'mount','--bind',path,path)
    readOnlyCapture={nodeId:pool.nodeId,path}
    await node(pool.nodeId,'mount','-o','remount,bind,ro',path)
    point=await api(`/environments/${environment.id}/recovery-points`,'POST',{name:`Before change ${run}`,expectedRevision:before.revision},201)
    const failed=await operation(point.operationId,'failed');assert.match(failed.error,/read-only file system/)
    assert.equal((await api(`/environments/${environment.id}/recovery-points`))[0].state,'failed')
    const after=await api(`/environments/${environment.id}/state`);assert.equal(after.revision,before.revision)
    for(const original of before.assets){const actual=after.assets.find(a=>a.assetId===original.assetId);assert.equal(actual.instanceId,original.instanceId);assert.equal(actual.state,original.state)}
    report.captureFailure={phase:failed.phase,error:failed.error}
    await node(pool.nodeId,'umount',path);readOnlyCapture=undefined
    await api(`/operations/${point.operationId}/retry`,'POST',undefined,202)
  })
  await step('环境恢复点捕获、原实例和运行状态恢复、修订不变',async()=>{
    await operation(point.operationId)
    await api(`/environments/${environment.id}/recovery-points`,'POST',{name:'Stale revision',expectedRevision:before.revision-1},409)
    point=(await api(`/environments/${environment.id}/recovery-points`))[0];report.point=point
    assert.equal(point.state,'ready');assert.equal(point.assetCount,4);assert(point.sizeBytes>0)
    const after=await api(`/environments/${environment.id}/state`);assert.equal(after.revision,before.revision)
    for(const original of before.assets){const actual=after.assets.find(a=>a.assetId===original.assetId);assert.equal(actual.instanceId,original.instanceId);assert.equal(actual.state,original.state)}
  })
  await step('容器可写层和卷原生还原核对；VM 全盘、NVRAM、TPM 固定数据核对',async()=>{
    const primary=[...workers.keys()].find(id=>!workers.get(id).host)
    for(const actual of before.assets){const directory=dir(actual),manifest=JSON.parse(await node(actual.nodeId,'cat',`${directory}/manifest.json`));assert.equal(manifest.execution.instanceId,actual.instanceId);assert.equal(manifest.environmentId,environment.id)
      if(manifest.execution.template.kind==='container'){
        if(actual.state==='suspended')await action('resume',actual.assetId)
        await node(actual.nodeId,'ctr','-n','netlab','tasks','exec','--exec-id',randomUUID(),actual.instanceId,'/bin/sh','-c','printf changed > /root/recovery-marker; printf changed > /data/marker')
        if(actual.nodeId===primary)await node(primary,'env',`NETLAB_REAL_RECOVERY=${directory}`,'GOCACHE=/root/.cache/go-build','GOMODCACHE=/root/go/pkg/mod','TMPDIR=/var/lib/netlab-dev/test-tmp','/usr/local/bin/go','-C','/mnt/d/newgz/netlab','test','-buildvcs=false','-tags','libvirt_dlopen','./internal/engine','-run','^TestRealRecoveryArchive$','-count=1','-v')
      }else{
        assert.equal(manifest.disks.length,2)
        for(const disk of manifest.disks)await node(actual.nodeId,'qemu-io','-f','qcow2','-c','read -P 0x59 0 4096',`${directory}/${disk.file}`)
        await node(actual.nodeId,'test','-s',`${directory}/nvram.fd`);await node(actual.nodeId,'test','-s',`${directory}/tpm.tar`)
      }
    }
  })
  await step('销毁环境保留恢复点；模板和存储引用仍阻止误删',async()=>{
    await action('destroy')
    await api(`/templates/${vmTemplate.id}`,'DELETE',undefined,409)
    for(const pool of pools)await api(`/storage-pools/${pool.id}`,'DELETE',undefined,409)
    const points=await api(`/environments/${environment.id}/recovery-points`);assert.equal(points[0].state,'ready')
    for(const actual of before.assets)await node(actual.nodeId,'test','-s',`${dir(actual)}/manifest.json`)
  })
  await step('离线删除真实失败，原任务重试；销毁状态保持不变',async()=>{
    stoppedNode=[...workers.keys()].find(id=>workers.get(id).host);await node(stoppedNode,'systemctl','stop','netlab-node-dev.service')
    const op=await api(`/environments/${environment.id}/recovery-points/${point.id}`,'DELETE',undefined,202);await operation(op.id,'failed')
    assert.equal((await api(`/environments/${environment.id}/state`)).status,'destroyed')
    assert.equal((await api(`/environments/${environment.id}/recovery-points`))[0].state,'deleting')
    const restarting=stoppedNode,restartTime=Date.now();await node(stoppedNode,'systemctl','start','netlab-node-dev.service');stoppedNode=undefined;await connected([restarting],restartTime)
    await api(`/operations/${op.id}/retry`,'POST',undefined,202);await operation(op.id)
    assert.deepEqual(await api(`/environments/${environment.id}/recovery-points`),[])
    assert.equal((await api(`/environments/${environment.id}/state`)).status,'destroyed')
    for(const actual of before.assets){await node(actual.nodeId,'test','!','-e',dir(actual));await node(actual.nodeId,'test','!','-e',`${dir(actual)}.pending`)}
    point=undefined
  })
  report.passed=true
} catch(error){report.passed=false;report.error=error.message}
finally {
  if(readOnlyCapture)try{await node(readOnlyCapture.nodeId,'umount',readOnlyCapture.path)}catch(e){report.cleanupErrors.push(e.message)}
  if(stoppedNode)try{await node(stoppedNode,'systemctl','start','netlab-node-dev.service')}catch(e){report.cleanupErrors.push(e.message)}
  try{await deletePoint()}catch(e){report.cleanupErrors.push(e.message)}
  if(environment)try{const state=await api(`/environments/${environment.id}/state`);if(state.status!=='destroyed')await action('destroy')}catch(e){report.cleanupErrors.push(e.message)}
  if(vmTemplate)try{const op=await api(`/templates/${vmTemplate.id}`,'DELETE',undefined,202);await operation(op.id)}catch(e){report.cleanupErrors.push(e.message)}
  for(const pool of pools)try{const op=await api(`/storage-pools/${pool.id}`,'DELETE',undefined,202);await operation(op.id);await node(pool.nodeId,'rm','-r','--',source)}catch(e){report.cleanupErrors.push(e.message)}
  try{const nodes=await api('/nodes');for(const expected of baseline ?? [])assert.deepEqual(nodes.find(n=>n.id===expected.id).reserved,expected.reserved)}catch(e){report.cleanupErrors.push(e.message)}
  if(report.cleanupErrors.length)report.passed=false
  report.finishedAt=new Date().toISOString();await writeFile('data/recovery-points-result.json',JSON.stringify(report,null,2));console.log(JSON.stringify({passed:report.passed,error:report.error,cleanupErrors:report.cleanupErrors}));if(!report.passed)process.exitCode=1
}
