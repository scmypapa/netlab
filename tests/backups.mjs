import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { readFile, writeFile } from "node:fs/promises";
import { openAsBlob } from "node:fs";
import { posix as path } from "node:path";
import { Agent, request } from "node:https";
import { delay } from "./guest-ssh.mjs";

const execute = promisify(execFile), run = randomUUID();
const includeMemory = process.env.NETLAB_RECOVERY_MEMORY === "1";
const base = "http://127.0.0.1:8090/api/v1";
const workers = JSON.parse(await readFile("D:/.cache/netlab/artifacts/multi-node-workers.json", "utf8"));
const root = `/var/lib/netlab-dev/test-tmp/backup-${run}`;
const repositoryConfig = process.env.NETLAB_BACKUP_REPOSITORY_CONFIG
  ? JSON.parse(await readFile(process.env.NETLAB_BACKUP_REPOSITORY_CONFIG, "utf8")) : { location: root + "/repository" };
const report = { startedAt: new Date().toISOString(), backend: repositoryConfig.location.startsWith("s3:") ? "s3" : "directory", includeMemory, steps: [], cleanupErrors: [], vmGuestVerified: false };
const pools = [], clones = [], directories = new Set();
const recovered = [];
const identities = new Set(), diskFiles = [];
let env, point, repository, backup, vmTemplate, containerTemplate, recoveryRepository;
let baseline;
const quote = (value) => `'${String(value).replaceAll("'", "'\"'\"'")}'`;
async function node(id, ...args) {
  const worker = workers.find((w) => w.nodeId === id);
  const command = worker.host ? ["ssh", "-i", worker.keyPath, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", `UserKnownHostsFile=${worker.keyPath}.hosts`, `root@${worker.host}`, args.map(quote).join(" ")] : args;
  return (await execute("wsl.exe", ["-d", "Ubuntu", "-u", "root", "--exec", ...command], { timeout: 180000, maxBuffer: 2**20 })).stdout;
}
function apiClient(address) {
  let cookie;
  async function api(path, method = "GET", body, status = 200) {
    const multipart = body instanceof FormData;
    const response = await fetch(address + path, { method, headers: { ...(multipart ? {} : {"Content-Type":"application/json"}), ...(cookie ? {Cookie:cookie} : {}) }, body: body === undefined ? undefined : multipart ? body : JSON.stringify(body) });
    if (response.headers.get("set-cookie")) cookie = response.headers.get("set-cookie").split(";")[0];
    const raw = await response.text();
    assert.equal(response.status, status, `${method} ${path}: ${raw}`);
    return raw ? JSON.parse(raw) : undefined;
  }
  async function operation(id, expected="succeeded") {
    for (const deadline=Date.now()+180000; Date.now()<deadline;) {
      const op=await api("/operations/"+id);
      if (["succeeded","failed","partially_applied"].includes(op.state)) { assert.equal(op.state,expected,op.error); return op; }
      await delay(150);
    }
    throw new Error("operation timeout: "+id);
  }
  async function action(environment, command, assetId) {
    return operation((await api(`/environments/${environment.id}${assetId ? '/assets/'+assetId : ''}/actions`,"POST",{action:command},202)).id);
  }
  return {api,operation,action};
}
const source = apiClient(base), { api, operation, action } = source;
const recovery = process.env.NETLAB_BACKUP_RECOVERY_API ? apiClient(process.env.NETLAB_BACKUP_RECOVERY_API) : undefined;
const nodeAgent = new Agent({ca:await readFile("data/pki/ca.crt"),cert:await readFile("data/pki/controller.crt"),key:await readFile("data/pki/controller.key")});
async function removeTemplateCache(worker, template) {
  const endpoint=baseline.find((node)=>node.id===worker.nodeId).endpoint;
  await new Promise((resolve,reject)=>{
    const call=request(endpoint+`/node/v1/templates/${template.id}/versions/${template.version}`,{method:"DELETE",agent:nodeAgent},(response)=>{
      let raw=""; response.on("data",(chunk)=>raw+=chunk); response.on("end",()=>response.statusCode===204 ? resolve() : reject(new Error("native template removal: "+response.statusCode+": "+raw)));
    }); call.on("error",reject); call.end();
  });
}
async function nativeSnapshots(credentials, group) {
  const variables = { RESTIC_REPOSITORY: repositoryConfig.location, RESTIC_PASSWORD: credentials.password,
    AWS_ACCESS_KEY_ID: credentials.accessKey ?? "", AWS_SECRET_ACCESS_KEY: credentials.secretKey ?? "", AWS_DEFAULT_REGION: credentials.region ?? "" };
  const env = { ...process.env, ...variables, WSLENV: [process.env.WSLENV, ...Object.keys(variables).map((name) => name + "/u")].filter(Boolean).join(":") };
  const result = await execute("wsl.exe", ["-d", "Ubuntu", "-u", "root", "--exec", "/usr/local/bin/restic", "--json", "--no-cache", "snapshots", "--tag", group], { env, timeout: 30000 });
  return JSON.parse(result.stdout);
}
async function uploadTemplate(definition, file, name) {
  const body=new FormData(); body.append("template",JSON.stringify(definition)); body.append("files",file,name);
  return api("/templates","POST",body,201);
}
async function step(name, executeStep) {
  const item={name,startedAt:new Date().toISOString()}, start=Date.now(); report.steps.push(item);
  try { await executeStep(); item.passed=true; } catch (error) { item.passed=false; item.error=error.message; throw error; }
  finally { item.durationMs=Date.now()-start; console.log(JSON.stringify(item)); }
}
async function vmDisk(asset) {
  const disks=(await node(asset.nodeId,"virsh","domblklist",asset.instanceId,"--details")).split("\n").map((line)=>line.match(/^\s*file\s+disk\s+\S+\s+(.+)$/)?.[1]).filter(Boolean);
  assert(disks.length,"VM has no disk"); return disks[0];
}
async function verify(environment, captured, backend=source) {
  const {api,action}=backend;
  const state=await api(`/environments/${environment.id}/state`);
  assert.equal(state.assets.length,4);
  if(includeMemory) for(const asset of state.assets) {
    const original=captured.get(asset.assetId);
    if(original.kind!=="vm") continue;
    const disk=await vmDisk(asset), memory=path.join(path.dirname(disk),"memory.save");
    if(environment.id===env.id) {
      assert.equal(asset.state,original.state);
      assert.equal((await node(asset.nodeId,"sha256sum",memory)).split(/\s+/)[0],original.memory);
    } else {
      assert.equal(asset.state,"stopped");
      await node(asset.nodeId,"test","!","-e",memory);
    }
  }
  await action(environment,"force-stop");
  for (const asset of state.assets) {
    identities.add(asset.instanceId);
    const source=captured.get(asset.assetId);
    if (source.kind==="container") {
      await action(environment,"start",asset.assetId);
      const value=await node(asset.nodeId,"ctr","-n","netlab","tasks","exec","--exec-id",randomUUID(),asset.instanceId,"/bin/sh","-c","cat /root/backup-marker /data/marker");
      assert.equal(value,"durable-datadurable-volume");
    } else {
      const disk = await vmDisk(asset); diskFiles.push({nodeId: asset.nodeId, path: disk});
      await node(asset.nodeId,"qemu-io","-f","qcow2","-c","read -P 0x59 0 4096",disk);
      assert((await node(asset.nodeId,"virsh","dumpxml",asset.instanceId)).includes("<tpm"));
    }
  }
  await action(environment,"force-stop");
}
const captured=new Map();
try {
  await api("/sessions/login","POST",JSON.parse(await readFile("data/dev-login.json","utf8")));
  baseline=await api("/nodes");
  const primary=workers.find((worker)=>!worker.host).nodeId;
  await step("双节点四个混合资产部署，写入真实容器、卷和 KVM 数据",async()=>{
    containerTemplate=await uploadTemplate({name:"Backup container "+run,kind:"container",os:"Linux",version:1,source:"image.tar",format:"oci",resources:{cpu:1,memoryMiB:64,diskGiB:1}},await openAsBlob("data/nginx.tar"),"image.tar");
    await operation(containerTemplate.operationId);
    const container=containerTemplate;
    await node(primary,"mkdir","-p",root);
    directories.add(primary);
    await node(primary,"qemu-img","create","-f","qcow2",root+"/base.qcow2","1G");
    const disk=(await execute("wsl.exe",["-d","Ubuntu","-u","root","--exec","cat",root+"/base.qcow2"],{encoding:"buffer",maxBuffer:2**20})).stdout;
    vmTemplate=await uploadTemplate({name:"Backup UEFI "+run,kind:"vm",os:"Linux",version:1,source:"backup.qcow2",format:"qcow2",resources:{cpu:1,memoryMiB:128,diskGiB:1},hardware:{machine:"q35",firmware:"uefi",secureBoot:true,tpm:true,diskBus:"virtio",nicModel:"virtio"}},new Blob([disk]),"backup.qcow2");
    await operation(vmTemplate.operationId);
    for(const worker of workers) {
      await node(worker.nodeId,"mkdir","-p",root+"/pool");
      directories.add(worker.nodeId);
      pools.push(await api("/storage-pools","POST",{nodeId:worker.nodeId,name:"Backup "+run,directory:root+"/pool"},201));
    }
    const network={id:randomUUID(),name:"Backup LAN",cidr:"192.168.93.0/24"};
    const assets=pools.flatMap((pool)=>[container,vmTemplate].map((template)=>({id:randomUUID(),name:template.kind+" "+pool.nodeId.slice(0,4),templateId:template.id,storagePoolId:pool.id,resources:template.resources,interfaces:[{id:randomUUID(),networkId:network.id,primary:true,mac:"",address:""}],volumes:[{id:"data",mountPath:"/data",sizeGiB:1}]})));
    env=await api("/environments","POST",{name:"Backup "+run,spec:{networks:[network],assets},run:true},201);
    await operation(env.operationId); await action(env,"force-stop");
    const state=await api(`/environments/${env.id}/state`);
    assert.equal(new Set(state.assets.map((asset)=>asset.nodeId)).size,2);
    for(const asset of state.assets) {
      identities.add(asset.instanceId);
      const kind=assets.find((source)=>source.id===asset.assetId).templateId===container.id?"container":"vm";
      captured.set(asset.assetId,{kind,instanceId:asset.instanceId,nodeId:asset.nodeId});
      if(kind==="container") {
        await action(env,"start",asset.assetId);
        await node(asset.nodeId,"ctr","-n","netlab","tasks","exec","--exec-id",randomUUID(),asset.instanceId,"/bin/sh","-c","printf durable-data > /root/backup-marker; printf durable-volume > /data/marker");
      } else {
        const disk=await vmDisk(asset); diskFiles.push({nodeId: asset.nodeId,path: disk});
        await node(asset.nodeId,"qemu-io","-f","qcow2","-c","write -P 0x59 0 4096",disk);
        if(includeMemory) {
          await action(env,"start",asset.assetId);
          if(asset.nodeId!==primary) await action(env,"suspend",asset.assetId);
          captured.get(asset.assetId).state=asset.nodeId===primary?"running":"suspended";
        }
      }
    }
    report.environmentId=env.id;
  });
  await step("捕获固定恢复点，备份至加密" + (report.backend === "s3" ? " S3" : "目录") + "仓库",async()=>{
    const state=await api(`/environments/${env.id}/state`);
    point=await api(`/environments/${env.id}/recovery-points`,"POST",{name:"Backup source",expectedRevision:state.revision,includeMemory},201); await operation(point.operationId);
    if(includeMemory) {
      assert.equal((await api(`/environments/${env.id}/recovery-points`)).find(item=>item.id===point.id).memoryAssetCount,2);
      for(const asset of state.assets.filter(item=>captured.get(item.assetId).kind==="vm")) {
        const files=(await node(asset.nodeId,"find",root+"/pool","-type","f","-path",`*/recovery-points/${point.id}/${asset.assetId}/memory.save`)).trim().split("\n");
        assert.equal(files.length,1); assert(files[0]);
        captured.get(asset.assetId).memory=(await node(asset.nodeId,"sha256sum",files[0])).split(/\s+/)[0];
      }
    }
    repository=await api("/backup-repositories","POST",{name:"Backup "+run,nodeId:primary,...repositoryConfig},201); await operation(repository.operationId);
    backup=await api(`/environments/${env.id}/backups`,"POST",{name:"Durable "+run,repositoryId:repository.id,recoveryPointId:point.id},201);
    const beforeBackup=await api(`/environments/${env.id}/state`);
    await operation(backup.operationId);
    const afterBackup=await api(`/environments/${env.id}/state`);
    assert.equal(afterBackup.status,beforeBackup.status); assert.notEqual(afterBackup.operation?.id,backup.operationId);
    const list=await api(`/environments/${env.id}/backups`); backup=list.find((item)=>item.id===backup.id); assert.equal(backup.state,"ready"); assert(backup.sizeBytes>0); report.sizeBytes=backup.sizeBytes;
  });
  await step("删除源恢复点、实例及原存储池；备份数据独立保留",async()=>{
    await operation((await api(`/environments/${env.id}/recovery-points/${point.id}`,"DELETE",undefined,202)).id); point=undefined;
    await action(env,"destroy");
    for(const pool of pools) await operation((await api("/storage-pools/"+pool.id,"DELETE",undefined,202)).id);
    assert.equal((await api(`/environments/${env.id}/recovery-points`)).length,0);
  });
  if(recovery) await step("独立控制面接入"+(report.backend === "s3" ? " S3" : "目录")+"仓库，原模板制品删除后恢复四个混合资产",async()=>{
    await recovery.api("/sessions/login","POST",JSON.parse(await readFile("data/dev-login.json","utf8")));
    assert.equal((await recovery.api("/environments")).length,0);
    assert.equal((await recovery.api("/templates")).length,0);
    for(const worker of workers) {
      await recovery.api("/nodes","POST",{endpoint:baseline.find((node)=>node.id===worker.nodeId).endpoint},201);
      for(const template of [containerTemplate,vmTemplate]) await removeTemplateCache(worker,template);
    }
    const credentials=await api("/backup-repositories/"+repository.id+"/credentials");
    recoveryRepository=await recovery.api("/backup-repositories","POST",{name:"Recovery "+run,nodeId:primary,...repositoryConfig,...credentials,initialize:false},201);
    await recovery.operation(recoveryRepository.operationId);
    const imported=await recovery.api(`/backup-repositories/${recoveryRepository.id}/backups`);
    assert.equal(imported.length,1); assert.equal(imported[0].id,backup.id); assert.equal(imported[0].environmentId,undefined);
    const clone=await recovery.api("/environments","POST",{name:"Rebuilt controller "+run,backupId:imported[0].id,run:false},201);
    recovered.push(clone); await recovery.operation(clone.operationId); await verify(clone,captured,recovery);
    const restoredTemplates=await recovery.api("/templates");
    assert.equal(restoredTemplates.length,2); assert(restoredTemplates.every((template)=>template.state==="ready"&&template.artifactNodeId===primary));
    await recovery.action(clone,"start"); await recovery.action(clone,"force-stop");
    await recovery.action(clone,"destroy");
    await recovery.api("/backup-repositories/"+recoveryRepository.id,"DELETE",undefined,204); recoveryRepository=undefined;
    assert.equal((await nativeSnapshots(credentials,"netlab:backup:"+backup.id)).length,7);
    for(const template of restoredTemplates) await recovery.operation((await recovery.api("/templates/"+template.id,"DELETE",undefined,202)).id);
    report.freshControllerRecovered=true;
  });
  await step("由备份克隆独立环境，核对四个资产与可写数据",async()=>{
    const clone=await api("/environments","POST",{name:"Backup clone "+run,backupId:backup.id,run:false},201); clones.push(clone); await operation(clone.operationId); await verify(clone,captured);
  });
  await step("销毁后由备份覆盖恢复，原实例身份与数据保持一致",async()=>{
    const state=await api(`/environments/${env.id}/state`);
    const task=await api(`/environments/${env.id}/backups/${backup.id}/restore`,"POST",{expectedRevision:state.revision},202); await operation(task.id);
    const restored=await api(`/environments/${env.id}/state`);
    for(const asset of restored.assets) assert.equal(asset.instanceId,captured.get(asset.assetId).instanceId);
    report.restoredOnDifferentNode=restored.assets.filter(asset=>asset.nodeId!==captured.get(asset.assetId).nodeId).length;
    await verify(env,captured);
  });
  await step("销毁、删除备份及仓库，检查数据、原生实例和容量释放",async()=>{
    for(const environment of [env,...clones]) await action(environment,"destroy");
    const credentials = await api("/backup-repositories/" + repository.id + "/credentials");
    const group = "netlab:backup:" + backup.id;
    assert.equal((await nativeSnapshots(credentials, group)).length, 7);
    await operation((await api(`/environments/${env.id}/backups/${backup.id}`,"DELETE",undefined,202)).id); backup=undefined;
    assert.equal((await api(`/environments/${env.id}/backups`)).length,0);
    assert.equal((await nativeSnapshots(credentials, group)).length, 0);
    await api("/backup-repositories/"+repository.id,"DELETE",undefined,204); repository=undefined;
    for(const environment of [env,...clones]) assert.equal((await api(`/environments/${environment.id}/state`)).assets.length,0);
    for(const disk of diskFiles) await node(disk.nodeId,"test","!","-e",disk.path);
    for(const worker of workers) {
      const domains=await node(worker.nodeId,"virsh","list","--all","--uuid");
      const containers=await node(worker.nodeId,"ctr","-n","netlab","containers","list","-q");
      const snapshots=await node(worker.nodeId,"ctr","-n","netlab","snapshots","list");
      for(const id of identities) { assert(!domains.includes(id)); assert(!containers.includes(id)); assert(!snapshots.includes(id)); }
      for(const environment of [env,...clones,...recovered]) {
        assert.equal((await node(worker.nodeId,"ovs-vsctl","--format=csv","--data=bare","--no-headings","--columns=name","find","Port","external_ids:netlab.environment="+environment.id)).trim(),"");
      }
    }
    for(const environment of [env,...clones,...recovered]) for(const table of ["Logical_Switch","Logical_Switch_Port","Logical_Router","Logical_Router_Port"]) {
      assert.equal((await node(primary,"ovn-nbctl","--db=unix:/run/ovn/ovnnb_db.sock","--format=csv","--data=bare","--no-headings","--columns=name","find",table,"external_ids:netlab.environment="+environment.id)).trim(),"");
    }
    const after=await api("/nodes");
    for(const before of baseline) assert.deepEqual(after.find((node)=>node.id===before.id).reserved,before.reserved);
    report.residualChecks={instances: identities.size,diskFiles:diskFiles.length,nodes:workers.length,ovnTables:4,ovsPorts:true,capacity:true,nativeSnapshots:true};
    report.passed=true;
  });
} catch(error) { report.passed=false; report.error=error.message; process.exitCode=1; }
finally {
  const cleanup=async(fn)=>{try{await fn();}catch(error){report.cleanupErrors.push(error.message);}};
  for(const environment of recovered) await cleanup(async()=>{const state=await recovery.api(`/environments/${environment.id}/state`); if(state.status!=="destroyed") await recovery.action(environment,"destroy");});
  if(recoveryRepository) await cleanup(()=>recovery.api("/backup-repositories/"+recoveryRepository.id,"DELETE",undefined,204));
  if(recovery) for(const template of (await recovery.api("/templates")).filter((item)=>[containerTemplate?.id,vmTemplate?.id].includes(item.id))) {
    await cleanup(async()=>recovery.operation((await recovery.api("/templates/"+template.id,"DELETE",undefined,202)).id));
  }
  for(const environment of [env,...clones].filter(Boolean)) await cleanup(async()=>{ const state=await api(`/environments/${environment.id}/state`); if(state.status!=="destroyed") await action(environment,"destroy"); });
  if(backup) await cleanup(async()=>operation((await api(`/environments/${env.id}/backups/${backup.id}`,"DELETE",undefined,202)).id));
  if(repository) await cleanup(()=>api("/backup-repositories/"+repository.id,"DELETE",undefined,204));
  if(point) await cleanup(async()=>operation((await api(`/environments/${env.id}/recovery-points/${point.id}`,"DELETE",undefined,202)).id));
  const remainingPools=await api("/storage-pools");
  for(const pool of pools) if(remainingPools.some((item)=>item.id===pool.id)) await cleanup(async()=>operation((await api("/storage-pools/"+pool.id,"DELETE",undefined,202)).id));
  if(vmTemplate) await cleanup(async()=>operation((await api("/templates/"+vmTemplate.id,"DELETE",undefined,202)).id));
  if(containerTemplate) await cleanup(async()=>operation((await api("/templates/"+containerTemplate.id,"DELETE",undefined,202)).id));
  if(!report.cleanupErrors.length) for(const id of directories) await cleanup(async()=>{
    const resolved=(await node(id,"realpath",root)).trim(); assert.equal(resolved,root); await node(id,"rm","-r","--",resolved);
  });
  report.finishedAt=new Date().toISOString(); report.durationMs=Date.parse(report.finishedAt)-Date.parse(report.startedAt);
  if(report.cleanupErrors.length){report.passed=false;process.exitCode=1;}
  await writeFile(process.env.NETLAB_BACKUP_REPORT??"data/backups-result.json",JSON.stringify(report,null,2));
  nodeAgent.destroy();
  console.log(JSON.stringify({passed:report.passed,steps:report.steps.length,cleanupErrors:report.cleanupErrors,durationMs:report.durationMs}));
}
