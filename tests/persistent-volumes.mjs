import assert from 'node:assert/strict'
import {randomUUID} from 'node:crypto'
import {execFile} from 'node:child_process'
import {promisify} from 'node:util'
import {readFile,writeFile} from 'node:fs/promises'
import {delay} from './guest-ssh.mjs'

const execute=promisify(execFile),run=randomUUID(),base=process.env.NETLAB_TEST_URL||'http://127.0.0.1:8090'
const workers=new Map(JSON.parse(await readFile('D:/.cache/netlab/artifacts/multi-node-workers.json','utf8')).map(worker=>[worker.nodeId,worker]))
const report={startedAt:new Date().toISOString(),steps:[],cleanupErrors:[]}
const environments=[],templates=[],points=[],volumeIds=new Set(),placement=[],volumeFiles=[]
let cookie,ceph,baseline,nodeBaseline
const quote=value=>`'${String(value).replaceAll("'","'\"'\"'")}'`
async function node(id,...args) {
  const worker=workers.get(id)
  const command=worker.host?['ssh','-i',worker.keyPath,'-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o',`UserKnownHostsFile=${worker.keyPath}.hosts`,`root@${worker.host}`,args.map(quote).join(' ')]:args
  return (await execute('wsl.exe',['-d','Ubuntu','-u','root','--exec',...command],{timeout:120000,maxBuffer:2**20})).stdout
}
async function api(path,method='GET',body,status=200) {
  const response=await fetch(`${base}/api/v1${path}`,{method,headers:{'Content-Type':'application/json',...(cookie?{Cookie:cookie}:{})},body:body===undefined?undefined:JSON.stringify(body)})
  if(response.headers.get('set-cookie'))cookie=response.headers.get('set-cookie').split(';')[0]
  const text=await response.text()
  assert.equal(response.status,status,`${method} ${path}: ${text}`)
  return text?JSON.parse(text):undefined
}
async function operation(id,expected='succeeded') {
  for(const end=Date.now()+180000;Date.now()<end;) {
    const result=await api(`/operations/${id}`)
    if(['succeeded','failed','partially_applied'].includes(result.state)){assert.equal(result.state,expected,`${result.phase}: ${result.error}`);return result}
    await delay(150)
  }
  throw new Error(`operation timeout: ${id}`)
}
async function step(name,fn) {
  const result={name,passed:false},start=performance.now()
  try{await fn();result.passed=true}catch(error){result.error=error.message;throw error}finally{result.durationMs=Math.round(performance.now()-start);report.steps.push(result);console.log(JSON.stringify(result))}
}
async function createVolume(kind,pool) {
  const op=await api('/volumes','POST',{name:`${kind} data ${run}`,kind,storagePoolId:pool,sizeGiB:1},202)
  const volume=(await api('/volumes')).find(item=>item.operationId===op.id)
  assert(volume)
  volumeIds.add(volume.id)
  await operation(op.id)
  const storage=(await api('/storage-pools')).find(item=>item.id===pool).storage
  if(!storage.rbd)volumeFiles.push({nodeId:volume.nodeId,path:`${storage.path}/volumes/${volume.id}${kind==='vm'?'.qcow2':''}`})
  return volume
}
async function action(env,kind){await operation((await api(`/environments/${env.id}/actions`,'POST',{action:kind},202)).id)}
async function change(env,spec){const current=await api(`/environments/${env.id}`);const result=await api(`/environments/${env.id}/changes`,'POST',{expectedRevision:current.revision,apply:true,spec},202);await operation(result.id)}
function asset(template,volume) {return {id:randomUUID(),name:template.kind,templateId:template.id,resources:template.resources,interfaces:[],volumes:[{id:'data',mountPath:template.kind==='vm'?'data':'/data',sizeGiB:1,persistentVolumeId:volume.id}]}}
async function createEnvironment(assets,networked=true) {
  const network={id:randomUUID(),name:'Data LAN',cidr:'192.168.86.0/24'}
  const env=await api('/environments','POST',{name:`Persistent data ${run}`,run:true,spec:{networks:networked?[network]:[],assets:networked?assets.map(item=>({...item,interfaces:[{id:randomUUID(),networkId:network.id,mac:'',address:'',primary:true}]})):assets}},201)
  environments.push(env)
  await operation(env.operationId)
  placement.push(...(await api(`/environments/${env.id}/state`)).assets)
  return env
}
async function diskSource(env,asset) {
  const current=(await api(`/environments/${env.id}/state`)).assets.find(item=>item.assetId===asset.id)
  const xml=await node(current.nodeId,'virsh','dumpxml',current.instanceId)
  const block=xml.match(/<disk\b[\s\S]*?<\/disk>/g)?.find(disk=>disk.includes('<serial>volume-data</serial>'))
  assert(block,'missing actual libvirt data disk')
  const file=block.match(/<source file='([^']+)'/)
  if(file)return {nodeId:current.nodeId,args:['-f','qcow2'],path:file[1]}
  const rbd=block.match(/<source protocol='rbd' name='([^']+)'/)
  assert(rbd,'missing native RBD source')
  const root=ceph.storage.path
  return {nodeId:current.nodeId,args:['-f','raw'],path:'json:'+JSON.stringify({driver:'raw',file:{driver:'rbd',pool:'netlab',image:rbd[1].slice('netlab/'.length),user:'netlab',conf:`${root}/ceph.conf`}})}
}
async function pattern(env,asset,write,value=0x5a) {
  const disk=await diskSource(env,asset)
  await node(disk.nodeId,'qemu-io',...disk.args,'-c',`${write?'write':'read'} -P ${value} 0 4096`,disk.path)
}
try {
  await api('/sessions/login','POST',JSON.parse(await readFile('data/dev-login.json','utf8')))
  let nodes=[]
  for(const end=Date.now()+20000;Date.now()<end;) {
    nodes=(await api('/nodes')).filter(item=>workers.has(item.id)&&item.state==='ready')
    if(nodes.length===workers.size)break
    await delay(250)
  }
  assert.equal(nodes.length,workers.size,'test workers did not return after service restart')
  baseline=await api('/storage-pools')
  nodeBaseline=new Map(nodes.map(node=>[node.id,node.reserved.diskGiB]))
  const primary=[...workers].find(([,worker])=>!worker.host)[0]
  for(const [id,worker] of workers) {
    await node(id,'qemu-img','create','-f','qcow2',`/var/lib/netlab-dev/test-tmp/volume-${run}.qcow2`,'1G')
    if(worker.host)await execute('wsl.exe',['-d','Ubuntu','-u','root','--exec','scp','-i',worker.keyPath,'-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o',`UserKnownHostsFile=${worker.keyPath}.hosts`,'/mnt/d/newgz/netlab/data/nginx.tar',`root@${worker.host}:/var/lib/netlab-dev/test-tmp/volume-${run}.tar`],{timeout:90000})
    else await node(id,'cp','/mnt/d/newgz/netlab/data/nginx.tar',`/var/lib/netlab-dev/test-tmp/volume-${run}.tar`)
  }
  for(const kind of ['container','vm']) {
    const template=await api('/templates','POST',{name:`Persistent ${kind} ${run}`,kind,os:'Linux',version:1,source:`/var/lib/netlab-dev/test-tmp/volume-${run}.${kind==='vm'?'qcow2':'tar'}`,format:kind==='vm'?'qcow2':'oci',resources:{cpu:1,memoryMiB:128,diskGiB:1},hardware:kind==='vm'?{machine:'q35',firmware:'bios',diskBus:'virtio',nicModel:'virtio'}:undefined},201)
    templates.push(template);await operation(template.operationId)
  }
  const containerVolume=await createVolume('container',`default:${primary}`)
  const vmNode=nodes.find(item=>item.id!==primary).id,vmVolume=await createVolume('vm',`default:${vmNode}`)
  const assets=[asset(templates[0],containerVolume),asset(templates[1],vmVolume)]
  let env
  await step('两个目录池的容器/VM 持久卷附加、自动放置、真实写入和单次容量计数',async()=>{
    env=await createEnvironment(assets)
    const state=await api(`/environments/${env.id}/state`)
    assert.equal(state.assets.find(item=>item.assetId===assets[0].id).nodeId,primary)
    assert.equal(state.assets.find(item=>item.assetId===assets[1].id).nodeId,vmNode)
    for(const item of state.assets)assert.equal(item.state,'running',item.error)
    const container=state.assets.find(item=>item.assetId===assets[0].id)
    await node(container.nodeId,'ctr','-n','netlab','tasks','exec','--exec-id',randomUUID(),container.instanceId,'/bin/sh','-c','printf durable > /data/marker')
    await action(env,'force-stop');await pattern(env,assets[1],true)
    const pools=await api('/storage-pools')
    const nodes=await api('/nodes')
    for(const id of [primary,vmNode]) {
      assert.equal(pools.find(item=>item.id===`default:${id}`).allocatedGiB,baseline.find(item=>item.id===`default:${id}`).allocatedGiB+2)
      assert.equal(nodes.find(item=>item.id===id).reserved.diskGiB,nodeBaseline.get(id)+2)
    }
    await api(`/volumes/${vmVolume.id}`,'DELETE',undefined,409)
    await api(`/volumes/${vmVolume.id}`,'PUT',{sizeGiB:2},409)
    await api('/environments','POST',{name:'Duplicate writer',spec:{networks:[],assets:[asset(templates[1],vmVolume)]},run:true},409)
  })
  await step('卸载、扩容、重新附加，保留容器文件和虚拟磁盘原始数据',async()=>{
    let current=await api(`/environments/${env.id}`),spec=structuredClone(current.appliedSpec)
    for(const asset of spec.assets)asset.volumes=[]
    await change(env,spec)
    for(const volume of [containerVolume,vmVolume])await operation((await api(`/volumes/${volume.id}`,'PUT',{sizeGiB:2},202)).id)
    spec=(await api(`/environments/${env.id}`)).appliedSpec
    for(const item of spec.assets)item.volumes=[{...assets.find(asset=>asset.id===item.id).volumes[0],sizeGiB:1}]
    await change(env,spec)
    current=await api(`/environments/${env.id}`)
    for(const item of current.appliedSpec.assets)assert.equal(item.volumes[0].sizeGiB,2)
    await pattern(env,assets[1],false)
    await action(env,'start')
    const container=(await api(`/environments/${env.id}/state`)).assets.find(item=>item.assetId===assets[0].id)
    assert.equal((await node(container.nodeId,'ctr','-n','netlab','tasks','exec','--exec-id',randomUUID(),container.instanceId,'/bin/cat','/data/marker')).trim(),'durable')
    await action(env,'force-stop')
  })
  await step('捕获和恢复生成独立持久卷副本，原卷及数据保持',async()=>{
    const current=await api(`/environments/${env.id}`)
    const point=await api(`/environments/${env.id}/recovery-points`,'POST',{name:'Persistent capture',expectedRevision:current.revision,includeMemory:false},201)
    points.push({env,id:point.id});await operation(point.operationId)
    await pattern(env,assets[1],true,0x6b)
    const pool=(await api('/storage-pools')).find(item=>item.id===`default:${vmNode}`)
    const volumeInventory=async()=>Promise.all([primary,vmNode].map(async id=>{
      const storage=baseline.find(item=>item.id===`default:${id}`).storage
      return (await node(id,'find',`${storage.path}/volumes`,'-mindepth','1','-maxdepth','1','-printf','%f\n')).trim().split('\n').sort()
    }))
    const beforeFailure=await volumeInventory()
    const manifest=`${pool.storage.path}/recovery-points/${point.id}/${assets[1].id}/manifest.json`
    await node(vmNode,'mv',manifest,`${manifest}.held`)
    try {
      const failed=await operation((await api(`/environments/${env.id}/recovery-points/${point.id}/restore`,'POST',{expectedRevision:current.revision},202)).id,'failed')
      assert.equal(failed.phase,'rolled-back',failed.error)
      assert.equal((await api('/volumes')).filter(item=>volumeIds.has(item.id)).length,2)
      assert.equal((await api('/volumes')).filter(item=>item.operationId===failed.id).length,0,'failed recovery left a volume registration')
      assert.deepEqual((await api(`/environments/${env.id}`)).appliedSpec.assets.map(item=>item.volumes[0].persistentVolumeId),[containerVolume.id,vmVolume.id])
      await pattern(env,assets[1],false,0x6b)
      assert.deepEqual(await volumeInventory(),beforeFailure,'failed recovery left physical volume data')
      report.recoveryFailure={phase:failed.phase,error:failed.error}
    } finally {await node(vmNode,'mv',`${manifest}.held`,manifest)}
    const op=await api(`/environments/${env.id}/recovery-points/${point.id}/restore`,'POST',{expectedRevision:current.revision},202)
    await operation(op.id)
    const restored=await api(`/environments/${env.id}`)
    const restoredIds=restored.appliedSpec.assets.map(item=>item.volumes[0].persistentVolumeId)
    for(const id of restoredIds){assert(id);assert(![containerVolume.id,vmVolume.id].includes(id));volumeIds.add(id);const volume=await api(`/volumes/${id}`),storage=(await api('/storage-pools')).find(item=>item.id===volume.storagePoolId).storage;volumeFiles.push({nodeId:volume.nodeId,path:`${storage.path}/volumes/${id}${volume.kind==='vm'?'.qcow2':''}`})}
    await pattern(env,assets[1],false)
    await operation((await api(`/environments/${env.id}/recovery-points/${point.id}`,'DELETE',undefined,202)).id)
    points.length=0
    // Derive the registered default directory from the storage API, never from a display name.
    await node(vmNode,'qemu-io','-f','qcow2','-c','read -P 107 0 4096',`${pool.storage.path}/volumes/${vmVolume.id}.qcow2`)
    await action(env,'destroy')
    assert.equal((await api('/volumes')).filter(item=>volumeIds.has(item.id)).length,4)
    const newAssets=[asset(templates[0],containerVolume),asset(templates[1],vmVolume)]
    const next=await createEnvironment(newAssets);await action(next,'force-stop');await pattern(next,newAssets[1],false,0x6b)
    await action(next,'destroy')
  })
  await step('容器与 VM 携独立卷跨节点停机迁移，保持身份、文件与磁盘数据',async()=>{
    const migratingAssets=[asset(templates[0],containerVolume),asset(templates[1],vmVolume)]
    const migrating=await createEnvironment(migratingAssets)
    await operation((await api(`/environments/${migrating.id}/assets/${migratingAssets[1].id}/actions`,'POST',{action:'force-stop'},202)).id)
    for(const item of migratingAssets) {
      const current=await api(`/environments/${migrating.id}`)
      const before=(await api(`/environments/${migrating.id}/state`)).assets.find(a=>a.assetId===item.id)
      const destination=[...workers.keys()].find(id=>id!==before.nodeId)
      const candidates=await api(`/environments/${migrating.id}/assets/${item.id}/migrations`)
      assert(candidates.some(candidate=>candidate.id===destination&&!candidate.live))
      await operation((await api(`/environments/${migrating.id}/assets/${item.id}/migrations`,'POST',{expectedRevision:current.revision,targetNodeId:destination,clientRequestId:randomUUID()},202)).id)
      const after=(await api(`/environments/${migrating.id}/state`)).assets.find(a=>a.assetId===item.id)
      assert.equal(after.nodeId,destination)
      assert.equal(after.instanceId,before.instanceId)
      assert.equal(after.state,before.state)
      assert.equal((await api(`/environments/${migrating.id}`)).revision,current.revision+1)
      const volume=await api(`/volumes/${item.volumes[0].persistentVolumeId}`)
      assert.equal(volume.nodeId,destination)
      assert.equal(volume.storagePoolId,`default:${destination}`)
      const storage=baseline.find(pool=>pool.id===volume.storagePoolId).storage
      volumeFiles.push({nodeId:destination,path:`${storage.path}/volumes/${volume.id}${volume.kind==='vm'?'.qcow2':''}`})
      if(item.templateId===templates[0].id)assert.equal((await node(destination,'ctr','-n','netlab','tasks','exec','--exec-id',randomUUID(),after.instanceId,'/bin/cat','/data/marker')).trim(),'durable')
      else await pattern(migrating,item,false,0x6b)
      const oldInventory=(await node(before.nodeId,'virsh','list','--all','--uuid'))+(await node(before.nodeId,'ctr','-n','netlab','containers','list','-q'))
      assert(!oldInventory.includes(before.instanceId),'migration left a source instance')
      placement.push(after)
    }
    await action(migrating,'destroy')
  })
  await step('共享 Ceph 卷创建、附加、RBD 写读、销毁保留和删除清理',async()=>{
    const key=(await execute('wsl.exe',['-d','Ubuntu','-u','root','--exec','ceph-authtool','/var/lib/netlab-dev/ceph-test/client.keyring','-n','client.netlab','--print-key'])).stdout.trim()
    ceph=await api('/storage-pools','POST',{name:`Persistent Ceph ${run}`,driver:'rbd',nodeIds:nodes.map(item=>item.id),ceph:{monitors:['192.168.122.1:16789'],pool:'netlab',user:'netlab',key}},201)
    const volume=await createVolume('vm',ceph.id),data=asset(templates[1],volume),env=await createEnvironment([data],false)
    await action(env,'force-stop');await pattern(env,data,true);await pattern(env,data,false)
    const current=await api(`/environments/${env.id}`)
    const point=await api(`/environments/${env.id}/recovery-points`,'POST',{name:'Persistent RBD capture',expectedRevision:current.revision,includeMemory:false},201)
    points.push({env,id:point.id});await operation(point.operationId)
    await pattern(env,data,true,0x6b)
    await operation((await api(`/environments/${env.id}/recovery-points/${point.id}/restore`,'POST',{expectedRevision:current.revision},202)).id)
    const copy=(await api(`/environments/${env.id}`)).appliedSpec.assets[0].volumes[0].persistentVolumeId
    assert(copy&&copy!==volume.id);volumeIds.add(copy)
    await pattern(env,data,false)
    await api(`/volumes/${volume.id}`,'DELETE',undefined,409)
    await operation((await api(`/environments/${env.id}/recovery-points/${point.id}`,'DELETE',undefined,202)).id)
    points.length=0
    const original='json:'+JSON.stringify({driver:'raw',file:{driver:'rbd',pool:'netlab',image:ceph.storage.rbd.imagePrefix+'volume-'+volume.id,user:'netlab',conf:`${ceph.storage.path}/ceph.conf`}})
    await node(volume.nodeId,'qemu-io','-f','raw','-c','read -P 107 0 4096',original)
    const before=(await api(`/environments/${env.id}/state`)).assets[0]
    const revision=(await api(`/environments/${env.id}`)).revision
    const destination=nodes.find(item=>item.id!==before.nodeId).id
    const cold=(await api(`/environments/${env.id}/assets/${data.id}/migrations`)).find(item=>item.id===destination)
    assert(cold&&!cold.live,'stopped RBD VM must use the cold migration lifecycle')
    await operation((await api(`/environments/${env.id}/assets/${data.id}/migrations`,'POST',{expectedRevision:revision,targetNodeId:destination,clientRequestId:randomUUID()},202)).id)
    const migrated=(await api(`/environments/${env.id}/state`)).assets[0]
    assert.equal(migrated.nodeId,destination);assert.equal(migrated.instanceId,before.instanceId);assert.equal(migrated.state,'stopped')
    assert.equal((await api(`/environments/${env.id}`)).revision,revision)
    assert.equal((await api('/volumes')).find(item=>item.id===copy).nodeId,destination)
    await pattern(env,data,false)
    await node(volume.nodeId,'qemu-io','-f','raw','-c','read -P 107 0 4096',original)
    report.sharedVolumeMigration={sourceNodeId:before.nodeId,targetNodeId:destination,instanceId:migrated.instanceId}
    await action(env,'destroy')
    assert((await api('/volumes')).some(item=>item.id===volume.id))
    for(const id of [volume.id,copy]){await operation((await api(`/volumes/${id}`,'DELETE',undefined,202)).id);volumeIds.delete(id)}
    const remaining=await node(volume.nodeId,'rbd','--conf',`${ceph.storage.path}/ceph.conf`,'--id','netlab','ls','netlab','--format','json')
    for(const id of [volume.id,copy,env.id])assert(!remaining.includes(id),'RBD image remains after cleanup')
    assert.equal((await api('/storage-pools')).find(item=>item.id===ceph.id).allocatedGiB,0)
  })
} catch(error) {report.failure=error.message;process.exitCode=1}
finally {
  for(const {env,id} of points)try{await operation((await api(`/environments/${env.id}/recovery-points/${id}`,'DELETE',undefined,202)).id)}catch(error){report.cleanupErrors.push(error.message)}
  for(const env of environments)try{if((await api(`/environments/${env.id}`)).status!=='destroyed')await action(env,'destroy');assert.equal((await api(`/environments/${env.id}/state`)).assets.length,0)}catch(error){report.cleanupErrors.push(error.message)}
  for(const id of volumeIds)try{if((await api('/volumes')).some(item=>item.id===id))await operation((await api(`/volumes/${id}`,'DELETE',undefined,202)).id)}catch(error){report.cleanupErrors.push(error.message)}
  if(ceph)try{await operation((await api(`/storage-pools/${ceph.id}`,'DELETE',undefined,202)).id)}catch(error){report.cleanupErrors.push(error.message)}
  for(const template of templates)try{await operation((await api(`/templates/${template.id}`,'DELETE',undefined,202)).id)}catch(error){report.cleanupErrors.push(error.message)}
  for(const id of workers.keys())try{await node(id,'rm','-f','--',`/var/lib/netlab-dev/test-tmp/volume-${run}.qcow2`,`/var/lib/netlab-dev/test-tmp/volume-${run}.tar`)}catch(error){report.cleanupErrors.push(error.message)}
  try{assert.equal((await api('/volumes')).filter(volume=>volumeIds.has(volume.id)).length,0)}catch(error){report.cleanupErrors.push(error.message)}
  for(const volume of volumeFiles)try{await node(volume.nodeId,'test','!','-e',volume.path)}catch(error){report.cleanupErrors.push(error.message)}
  for(const id of workers.keys())try {
    const instances=(await node(id,'virsh','list','--all','--uuid'))+(await node(id,'ctr','-n','netlab','containers','list','-q'))
    for(const asset of placement.filter(item=>item.nodeId===id))assert(!instances.includes(asset.instanceId),'instance remains after destroy')
    const ovs=await node(id,'ovs-vsctl','--format=json','--columns=external_ids','list','Interface')
    for(const env of environments)assert(!ovs.includes(env.id),'OVS interface remains after destroy')
  }catch(error){report.cleanupErrors.push(error.message)}
  if(report.cleanupErrors.length)process.exitCode=1
  report.finishedAt=new Date().toISOString();await writeFile('data/persistent-volumes-result.json',JSON.stringify(report,null,2));console.log(JSON.stringify({failure:report.failure,cleanupErrors:report.cleanupErrors}))
}
