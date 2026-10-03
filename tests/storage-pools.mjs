import assert from 'node:assert/strict'
import {randomUUID} from 'node:crypto'
import {execFile} from 'node:child_process'
import {promisify} from 'node:util'
import {readFile, writeFile} from 'node:fs/promises'
import {delay} from './guest-ssh.mjs'

const execute=promisify(execFile),run=randomUUID(),base='http://127.0.0.1:8090',second='http://127.0.0.1:8091'
const workers=new Map(JSON.parse(await readFile('D:/.cache/netlab/artifacts/multi-node-workers.json','utf8')).map(worker=>[worker.nodeId,worker]))
const source=`/var/lib/netlab-dev/test-tmp/storage-${run}`
const report={startedAt:new Date().toISOString(),steps:[],environments:[],pools:[],cleanupErrors:[]}
let cookie,baseline,environment,images=[],pools=[],inventory=[],blueprint,stoppedNode
const quote=value=>`'${String(value).replaceAll("'","'\"'\"'")}'`
async function node(id,...args) {
  const worker=workers.get(id)
  const command=worker.host ? ['ssh','-i',worker.keyPath,'-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o',`UserKnownHostsFile=${worker.keyPath}.hosts`,`root@${worker.host}`,args.map(quote).join(' ')] : args
  return (await execute('wsl.exe',['-d','Ubuntu','-u','root','--exec',...command],{timeout:90000,maxBuffer:2**20})).stdout
}
async function api(path,method='GET',body,status=200) {
  const response=await fetch(`${method==='DELETE' ? second : base}/api/v1${path}`,{method,headers:{'Content-Type':'application/json',...(cookie ? {Cookie:cookie} : {})},body:body===undefined ? undefined : JSON.stringify(body)})
  if(response.headers.get('set-cookie'))cookie=response.headers.get('set-cookie').split(';')[0]
  const text=await response.text()
  assert.equal(response.status,status,`${method} ${path}: ${text}`)
  return text ? JSON.parse(text) : undefined
}
async function operation(id,expected='succeeded') {
  for(const deadline=Date.now()+90000;Date.now()<deadline;) {
    const op=await api(`/operations/${id}`)
    if(['succeeded','failed','partially_applied'].includes(op.state)){assert.equal(op.state,expected,`${op.phase}: ${op.error}`);return op}
    await delay(150)
  }
  throw new Error(`operation timeout: ${id}`)
}
async function connected(ids,after) {
  for(const deadline=Date.now()+30000;Date.now()<deadline;) {
    const nodes=await api('/nodes')
    if(ids.every(id=>nodes.some(n=>n.id===id && n.state==='ready' && Date.parse(n.observedAt)>after)))return
    await delay(200)
  }
  throw new Error('nodes did not reconnect')
}
async function action(kind,assetId){const op=await api(`/environments/${environment.id}${assetId ? `/assets/${assetId}` : ''}/actions`,'POST',{action:kind},202);return operation(op.id)}
async function step(name,fn){const result={name,passed:false},start=performance.now();try{await fn();result.passed=true}catch(error){result.error=error.message;throw error}finally{result.durationMs=Math.round(performance.now()-start);report.steps.push(result);console.log(JSON.stringify(result))}}
function poolFor(actual){return inventory.find(p=>p.id===(actual.storagePoolId || `default:${actual.nodeId}`))}
async function deletePool(pool){const current=(await api('/storage-pools')).find(p=>p.id===pool.id);if(!current)return;let op=current.state==='deleting' ? await api(`/operations/${current.operationId}`) : await api(`/storage-pools/${pool.id}`,'DELETE',undefined,202);if(op.state==='failed')op=await api(`/operations/${op.id}/retry`,'POST',undefined,202);await operation(op.id)}
try {
  await api('/sessions/login','POST',JSON.parse(await readFile('data/dev-login.json','utf8')))
  for(const worker of workers.values())await api('/nodes','POST',{name:worker.host ? 'Worker-2' : 'Local node',endpoint:worker.address},201)
  baseline=await api('/nodes')
  const primary=[...workers].find(([,worker])=>!worker.host)[0]
  await step('双节点登记存储目录，查询真实容量，同盘目录使用相同文件系统身份',async()=>{
    for(const id of workers.keys()) {
      await node(id,'mkdir','-p',`${source}/data-a`,`${source}/data-b`)
      await node(id,'touch',`${source}/retained-source`)
      for(const directory of ['data-a','data-b']) {
        const pool=await api('/storage-pools','POST',{nodeId:id,name:`Storage ${directory} ${run}`,directory:`${source}/${directory}`},201)
        pools.push(pool);report.pools.push({id:pool.id,nodeId:id,path:pool.storage.path,filesystem:pool.storage.filesystem})
      }
      const own=pools.filter(p=>p.nodeId===id)
      assert.equal(own[0].storage.filesystem,own[1].storage.filesystem)
      assert(own[0].storage.availableBytes>0)
    }
    await api('/storage-pools','POST',{nodeId:primary,name:'Duplicate',directory:`${source}/data-a`},409)
    inventory=await api('/storage-pools')
  })
  await step('真实容器与 KVM 模板，双节点混合创建；指定和自动存储均按实际落点核对',async()=>{
    for(const [id,worker] of workers) {
      if(worker.host)await execute('wsl.exe',['-d','Ubuntu','-u','root','--exec','scp','-i',worker.keyPath,'-o','BatchMode=yes','-o','StrictHostKeyChecking=yes','-o',`UserKnownHostsFile=${worker.keyPath}.hosts`,'/mnt/d/newgz/netlab/data/nginx.tar',`root@${worker.host}:${source}/container.tar`],{timeout:90000})
      else await node(id,'cp','/mnt/d/newgz/netlab/data/nginx.tar',`${source}/container.tar`)
      await node(id,'qemu-img','create','-f','qcow2',`${source}/system.qcow2`,'1G')
    }
    for(const kind of ['container','vm']) {
      const image=await api('/templates','POST',{name:`Storage ${kind} ${run}`,kind,os:'Linux',version:1,source:`${source}/${kind==='vm' ? 'system.qcow2' : 'container.tar'}`,format:kind==='vm' ? 'qcow2' : 'oci',resources:{cpu:1,memoryMiB:128,diskGiB:1},hardware:kind==='vm' ? {machine:'q35',firmware:'bios',diskBus:'virtio',nicModel:'virtio'} : undefined},201)
      images.push(image);await operation(image.operationId)
    }
    const network={id:randomUUID(),name:'Storage LAN',cidr:'192.168.83.0/24'}
    const asset=(image,pool)=>({id:randomUUID(),name:`${image.kind}-${randomUUID().slice(0,4)}`,templateId:image.id,storagePoolId:pool?.id,resources:image.resources,interfaces:[{id:randomUUID(),networkId:network.id,mac:'',address:'',primary:true}],volumes:[{id:'data',mountPath:'/data',sizeGiB:1}]})
    const assets=[...workers.keys()].flatMap(id=>images.map(image=>asset(image,pools.find(p=>p.nodeId===id))))
    assets.push(asset(images[0]))
    environment=await api('/environments','POST',{name:`Storage lifecycle ${run}`,spec:{networks:[network],assets},run:true},201);report.environments.push(environment.id)
    await operation(environment.operationId)
    const state=await api(`/environments/${environment.id}/state`);report.placement=state.assets
    assert.equal(state.assets.length,5)
    for(const actual of state.assets) {
      const requested=assets.find(a=>a.id===actual.assetId),pool=poolFor(actual)
      if(requested.storagePoolId){assert.equal(actual.nodeId,pool.nodeId);assert.equal(actual.storagePoolId,requested.storagePoolId)}
      await node(actual.nodeId,'test','-d',`${pool.storage.path}/environments/${environment.id}/instances/${actual.instanceId}`)
      assert.equal(actual.state,'running',actual.error)
    }
    blueprint=await api(`/environments/${environment.id}/blueprints`,'POST',{name:`Storage blueprint ${run}`,expectedRevision:state.revision,spec:(await api(`/environments/${environment.id}`)).appliedSpec},201)
    await api(`/storage-pools/${assets[0].storagePoolId}`,'DELETE',undefined,409)
  })
  await step('容器数据卷与 VM 数据盘写入，保卷重建后检查原数据和存储归属',async()=>{
    const before=await api(`/environments/${environment.id}/state`),full=await api(`/environments/${environment.id}`)
    for(const actual of before.assets) {
      const asset=full.appliedSpec.assets.find(a=>a.id===actual.assetId)
      if(images.find(i=>i.id===asset.templateId).kind==='container')await node(actual.nodeId,'ctr','-n','netlab','tasks','exec','--exec-id',randomUUID(),actual.instanceId,'/bin/sh','-c','printf durable > /data/marker')
    }
    await action('force-stop')
    for(const actual of before.assets) {
      const asset=full.appliedSpec.assets.find(a=>a.id===actual.assetId),pool=poolFor(actual)
      if(images.find(i=>i.id===asset.templateId).kind==='vm')await node(actual.nodeId,'qemu-io','-f','qcow2','-c','write -P 0x59 0 4096',`${pool.storage.path}/environments/${environment.id}/volumes/${asset.id}/data.qcow2`)
      await action('rebuild',actual.assetId)
    }
    const state=await api(`/environments/${environment.id}/state`)
    for(const actual of state.assets) {
      const asset=full.appliedSpec.assets.find(a=>a.id===actual.assetId),pool=poolFor(actual)
      if(images.find(i=>i.id===asset.templateId).kind==='vm')await node(actual.nodeId,'qemu-io','-f','qcow2','-c','read -P 0x59 0 4096',`${pool.storage.path}/environments/${environment.id}/volumes/${asset.id}/data.qcow2`)
      else assert.equal((await node(actual.nodeId,'cat',`${pool.storage.path}/environments/${environment.id}/volumes/${asset.id}/data/marker`)).trim(),'durable')
      assert.equal(actual.storagePoolId,before.assets.find(a=>a.assetId===actual.assetId).storagePoolId)
    }
    await action('start')
  })
  await step('普通变更保持存储位置；节点重启接管非默认存储内的真实资产',async()=>{
    const full=await api(`/environments/${environment.id}`),forbidden=structuredClone(full.appliedSpec)
    const fixed=forbidden.assets.find(a=>a.storagePoolId),pool=pools.find(p=>p.id===fixed.storagePoolId)
    fixed.storagePoolId=pools.find(p=>p.nodeId===pool.nodeId && p.id!==pool.id).id
    await api(`/environments/${environment.id}/changes`,'POST',{expectedRevision:full.revision,apply:false,spec:forbidden},400)
    const before=await api(`/environments/${environment.id}/state`),after=Date.now()
    for(const id of workers.keys())await node(id,'systemctl','restart','netlab-node-dev.service')
    await connected([...workers.keys()],after)
    const state=await api(`/environments/${environment.id}/state`)
    for(const actual of state.assets) {
      assert.equal(actual.instanceId,before.assets.find(a=>a.assetId===actual.assetId).instanceId)
      assert.equal(actual.state,'running',actual.error)
      const asset=full.appliedSpec.assets.find(a=>a.id===actual.assetId)
      if(images.find(i=>i.id===asset.templateId).kind==='container')assert.equal((await node(actual.nodeId,'ctr','-n','netlab','tasks','exec','--exec-id',randomUUID(),actual.instanceId,'/bin/cat','/data/marker')).trim(),'durable')
    }
  })
  await step('销毁释放磁盘，版本引用继续生效；节点离线删除失败后沿原任务重试',async()=>{
    await action('destroy')
    await api(`/storage-pools/${pools[0].id}`,'DELETE',undefined,409)
    await api(`/blueprints/${blueprint.id}`,'DELETE',undefined,204);blueprint=undefined
    const secondary=[...workers].find(([,w])=>w.host)[0],pool=pools.find(p=>p.nodeId===secondary)
    await node(secondary,'systemctl','stop','netlab-node-dev.service');stoppedNode=secondary
    const op=await api(`/storage-pools/${pool.id}`,'DELETE',undefined,202)
    await operation(op.id,'failed')
    const current=(await api('/storage-pools')).find(p=>p.id===pool.id);assert.equal(current.state,'deleting');assert(current.error)
    const after=Date.now()
    await node(secondary,'systemctl','start','netlab-node-dev.service');stoppedNode=undefined
    await connected([secondary],after)
    const retry=await api(`/operations/${op.id}/retry`,'POST',undefined,202);assert.equal(retry.id,op.id);await operation(op.id)
    for(const pool of pools)await deletePool(pool)
  })
  await step('双节点实例、磁盘、OVN/OVS和存储池清理，原目录与外部文件保留',async()=>{
    const nodes=await api('/nodes');for(const previous of baseline)assert.deepEqual(nodes.find(n=>n.id===previous.id).reserved,previous.reserved)
    for(const pool of pools){await node(pool.nodeId,'test','!','-e',pool.storage.path);await node(pool.nodeId,'test','-d',pool.directory);await node(pool.nodeId,'test','-f',`${source}/retained-source`)}
    assert(!(await api('/storage-pools')).some(p=>pools.some(old=>p.id===old.id)))
    for(const image of images){const op=await api(`/templates/${image.id}`,'DELETE',undefined,202);await operation(op.id)}
    for(const id of workers.keys()) {
      const instances=(await node(id,'virsh','list','--all','--uuid'))+(await node(id,'ctr','-n','netlab','containers','list','-q'))
      for(const actual of report.placement.filter(a=>a.nodeId===id))assert(!instances.includes(actual.instanceId))
      assert(!(await node(id,'ovs-vsctl','--format=json','--columns=external_ids','list','Interface')).includes(environment.id))
    }
    for(const table of ['Logical_Switch','Logical_Router','Logical_Switch_Port','Logical_Router_Port'])assert(!(await node(primary,'ovn-nbctl','--format=json','--columns=external_ids','list',table)).includes(environment.id))
    report.passed=true
  })
}catch(error){report.passed=false;report.error=error.stack;process.exitCode=1;console.error(error.message)}
finally {
  const cleanup=async(fn)=>{try{await fn()}catch(error){report.cleanupErrors.push(error.message);report.passed=false;process.exitCode=1}}
  if(stoppedNode)await cleanup(()=>node(stoppedNode,'systemctl','start','netlab-node-dev.service'))
  if(environment)await cleanup(async()=>{if((await api(`/environments/${environment.id}`)).status!=='destroyed')await action('destroy')})
  if(blueprint)await cleanup(()=>api(`/blueprints/${blueprint.id}`,'DELETE',undefined,204))
  for(const pool of pools)await cleanup(()=>deletePool(pool))
  for(const image of images)await cleanup(async()=>{const[current]=await api(`/templates?ids=${image.id}`);if(current){let op=current.state==='deleting' ? await api(`/operations/${current.operationId}`) : await api(`/templates/${image.id}`,'DELETE',undefined,202);if(op.state==='failed')op=await api(`/operations/${op.id}/retry`,'POST',undefined,202);await operation(op.id)}})
  if(report.passed)for(const id of workers.keys())await cleanup(()=>node(id,'rm','-r','--',source))
  report.finishedAt=new Date().toISOString();await writeFile('data/storage-pools-result.json',JSON.stringify(report,null,2)+'\n')
}
