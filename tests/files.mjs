import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { writeFile } from 'node:fs/promises'
import { createRequire } from 'node:module'
import { guestKey, delay, wsl } from './guest-ssh.mjs'

const {chromium}=createRequire(new URL('../web/package.json',import.meta.url))('@playwright/test')
const base=process.env.NETLAB_TEST_URL||'http://127.0.0.1:8090'
const report={startedAt:new Date().toISOString(),steps:[],cleanupErrors:[]}
const environments=[]
let cookie,key,template,createdTemplate=false,fileToken
const headers=token=>token?{Authorization:'Bearer '+token}:{Cookie:cookie||''}
async function raw(path,method='GET',body,token){return fetch(base+'/api/v1'+path,{method,headers:{...headers(token),...(body===undefined?{}:{'Content-Type':'application/json'})},body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(150_000)})}
async function api(path,method='GET',body,token){const response=await raw(path,method,body,token);if(path==='/sessions/login')cookie=response.headers.get('set-cookie').split(';')[0];const value=response.status===204?undefined:await response.json();assert.ok(response.ok,method+' '+path+': '+response.status+' '+JSON.stringify(value));return value}
async function completed(id){const deadline=Date.now()+120_000;while(Date.now()<deadline){const result=await api('/operations/'+id);if(['succeeded','failed','partially_applied'].includes(result.state)){assert.equal(result.state,'succeeded',result.error);return}await delay(200)}throw new Error('operation timed out')}
async function step(name,run){const start=performance.now();const entry={name,passed:false};try{await run();entry.passed=true}catch(error){entry.error=error.message;entry.stack=error.stack;throw error}finally{entry.durationMs=Math.round(performance.now()-start);report.steps.push(entry);console.log(JSON.stringify(entry))}}
const assetPath=(environment,asset)=>'/environments/'+environment.id+'/assets/'+asset.id
async function command(environment,asset,path,action,destination,token){return api(assetPath(environment,asset)+'/files?path='+encodeURIComponent(path),'POST',{action,destination},token)}
async function put(environment,asset,path,value,token){const response=await fetch(base+'/api/v1'+assetPath(environment,asset)+'/files/content?path='+encodeURIComponent(path),{method:'PUT',headers:{...headers(token),'Content-Type':'application/octet-stream'},body:value,signal:AbortSignal.timeout(30_000)});assert.equal(response.status,204,await response.text())}
async function get(environment,asset,path,token){const response=await fetch(base+'/api/v1'+assetPath(environment,asset)+'/files/content?path='+encodeURIComponent(path),{headers:headers(token),signal:AbortSignal.timeout(30_000)});if(response.status!==200)throw new Error(response.status+' '+await response.text());return Buffer.from(await response.arrayBuffer())}
async function action(environment,asset,action){await completed((await api(assetPath(environment,asset)+'/actions','POST',{action,clientRequestId:randomUUID()})).id)}

try{
 await step('两个重叠网段环境、真实容器和 Ubuntu VM',async()=>{
  assert.ok(process.env.NETLAB_TEST_PASSWORD,'set NETLAB_TEST_PASSWORD')
  await api('/sessions/login','POST',{name:'admin',password:process.env.NETLAB_TEST_PASSWORD})
  key=guestKey()
  const templates=await api('/templates?limit=100')
  const container=templates.find(item=>item.kind==='container'&&item.state==='ready'&&item.name==='API container')
  assert.ok(container,'prepared container template missing')
  const source=process.env.NETLAB_TEST_LINUX_SOURCE||'/var/lib/netlab-dev/templates/ubuntu-24.04-guest.qcow2'
  template=templates.find(item=>item.kind==='vm'&&item.state==='ready'&&item.source===source)
  if(!template){createdTemplate=true;template=await api('/templates','POST',{id:randomUUID(),name:'SSH file verification',kind:'vm',os:'Ubuntu 24.04',version:1,source,initialization:'cloud-init',resources:{cpu:2,memoryMiB:1024,diskGiB:8},hardware:{firmware:'bios',machine:'pc',diskBus:'virtio',nicModel:'virtio'}});const deadline=Date.now()+120_000;while(Date.now()<deadline){template=(await api('/templates?ids='+template.id))[0];assert.notEqual(template.state,'failed',template.error);if(template.state==='ready')break;await delay(200)}assert.equal(template.state,'ready')}
  for(const label of ['A','B']){
   const network={id:randomUUID(),name:'LAN',cidr:'10.91.0.0/24'}
   const make=(name,template)=>({id:randomUUID(),name,templateId:template.id,resources:template.resources,interfaces:[{id:randomUUID(),networkId:network.id,mac:'',address:'',primary:true}]})
   const vm=make('Linux '+label,template);vm.guest={username:'netlab',sshAuthorizedKeys:[key.publicKey],hostname:'netlab-'+label.toLowerCase()}
   const docker=make('Container '+label,container);docker.volumes=[{id:randomUUID(),mountPath:'/data',sizeGiB:1}]
   const result=await api('/environments','POST',{name:'Files '+label,run:true,clientRequestId:randomUUID(),spec:{networks:[network],assets:[docker,vm]}})
   environments.push({id:result.id,vm,docker,label});await completed(result.operationId)
  }
 })
 await step('容器上传下载、中文名称、重命名和移动',async()=>{
  const environment=environments[0],asset=environment.docker
  await command(environment,asset,'/data/files','mkdir')
  await put(environment,asset,'/data/files/中文.txt','真实容器文件')
  const entries=await api(assetPath(environment,asset)+'/files?path=/data/files')
  assert.equal(entries[0].name,'中文.txt');assert.equal(entries[0].kind,'file');assert.ok(entries[0].modifiedAt)
  await command(environment,asset,'/data/files/中文.txt','rename','/data/renamed.txt')
  assert.equal((await get(environment,asset,'/data/renamed.txt')).toString(),'真实容器文件')
  const collision=await raw(assetPath(environment,asset)+'/files?path=/data/files','POST',{action:'rename',destination:'/data/renamed.txt'})
  assert.equal(collision.status,409)
 })
 await step('停止容器仍可操作真实 snapshot 和受管卷',async()=>{
  const environment=environments[0],asset=environment.docker
  await action(environment,asset,'stop')
  assert.equal((await get(environment,asset,'/data/renamed.txt')).toString(),'真实容器文件')
  await put(environment,asset,'/root/stopped.txt','stopped snapshot')
  await put(environment,asset,'/data/renamed.txt','volume while stopped')
  await action(environment,asset,'start')
  assert.equal((await get(environment,asset,'/root/stopped.txt')).toString(),'stopped snapshot')
  assert.equal((await get(environment,asset,'/data/renamed.txt')).toString(),'volume while stopped')
 })
 await step('普通环境 SSH 接入和独立主机密钥确认',async()=>{
  const privateKey=wsl('cat',key.path)
  for(const environment of environments){
   const deadline=Date.now()+120_000;let result,last
   while(Date.now()<deadline){const response=await raw(assetPath(environment,environment.vm)+'/ssh/host-key','POST',{port:22});if(response.ok){result=await response.json();break}last=await response.text();await delay(500)}
   assert.ok(result,last);assert.match(result.fingerprint,/^SHA256:/)
   environment.settings={username:'netlab',port:22,authKind:'key',hostKey:result.fingerprint,privateKey}
   await api(assetPath(environment,environment.vm)+'/ssh','PUT',environment.settings)
   const publicSettings=await api(assetPath(environment,environment.vm)+'/ssh')
   assert.equal(publicSettings.privateKey,undefined);assert.equal(publicSettings.password,undefined);assert.equal(publicSettings.passphrase,undefined)
  }
  assert.notEqual(environments[0].settings.hostKey,environments[1].settings.hostKey,'independent guests must have independent host keys')
 })
 await step('SFTP 流式传输与重叠 CIDR 隔离',async()=>{
  for(const environment of environments){await command(environment,environment.vm,'/home/netlab/files','mkdir');await put(environment,environment.vm,'/home/netlab/files/marker.txt',environment.label)}
  for(const environment of environments){assert.equal((await get(environment,environment.vm,'/home/netlab/files/marker.txt')).toString(),environment.label)}
  const environment=environments[0],asset=environment.vm
  const payload=Buffer.alloc(8<<20,0x65);payload.write('streaming payload')
  const start=performance.now();await put(environment,asset,'/home/netlab/files/large.bin',payload);assert.deepEqual(await get(environment,asset,'/home/netlab/files/large.bin'),payload);report.transfer={bytes:payload.length,roundTripMs:Math.round(performance.now()-start)}
  await command(environment,asset,'/home/netlab/files/marker.txt','rename','/home/netlab/files/renamed.txt')
  assert.equal((await get(environment,asset,'/home/netlab/files/renamed.txt')).toString(),'A')
 })
 await step('密钥变化拒绝和保存时保留既有认证信息',async()=>{
  const environment=environments[0],asset=environment.vm
  const publicSettings=await api(assetPath(environment,asset)+'/ssh')
  await api(assetPath(environment,asset)+'/ssh','PUT',{...publicSettings,hostKey:'SHA256:changed'})
  const refused=await raw(assetPath(environment,asset)+'/files?path=/home/netlab/files')
  assert.equal(refused.status,502);assert.match((await refused.json()).detail,/主机密钥已变化/)
  await api(assetPath(environment,asset)+'/ssh','PUT',publicSettings)
  assert.ok((await api(assetPath(environment,asset)+'/files?path=/home/netlab/files')).length)
 })
 await step('个人凭据隔离、文件授权和会话权限分离',async()=>{
  const environment=environments[0],asset=environment.vm
  fileToken=await api('/service-tokens','POST',{name:'File test '+randomUUID(),grants:[{scopeKind:'asset',scopeId:environment.id+'/'+asset.id,permissions:['read','file']}]})
  assert.equal((await raw(assetPath(environment,asset)+'/ssh','GET',undefined,fileToken.token)).status,404)
  await api(assetPath(environment,asset)+'/ssh','PUT',environment.settings,fileToken.token)
  assert.equal((await get(environment,asset,'/home/netlab/files/renamed.txt',fileToken.token)).toString(),'A')
  assert.equal((await raw(assetPath(environment,asset)+'/console?kind=ssh','GET',undefined,fileToken.token)).status,403)
  assert.equal((await raw(assetPath(environments[1],environments[1].vm)+'/files?path=/','GET',undefined,fileToken.token)).status,403)
 })
 await step('传输中撤销授权、原文件与临时文件清理',async()=>{
  const environment=environments[0],asset=environment.vm,abort=new AbortController()
  let settled=false,timedOut=false
  const body=new ReadableStream({start(controller){controller.enqueue(Buffer.alloc(65536,0x66))}})
  const pending=fetch(base+'/api/v1'+assetPath(environment,asset)+'/files/content?path=/home/netlab/files/renamed.txt',{method:'PUT',headers:{...headers(fileToken.token),'Content-Type':'application/octet-stream'},body,duplex:'half',signal:abort.signal}).then(response=>({status:response.status}),error=>({error:error.message})).finally(()=>{settled=true})
  const timer=setTimeout(()=>{timedOut=true;abort.abort()},7000)
  try{
   const deadline=Date.now()+2500;let temp=false
   while(Date.now()<deadline){temp=(await api(assetPath(environment,asset)+'/files?path=/home/netlab/files')).some(item=>item.name.startsWith('.netlab-upload-'));if(temp)break;await delay(50)}
   assert.ok(temp,'upload never reached guest staging file');assert.equal(settled,false)
   await api('/service-tokens/'+fileToken.principal.id,'DELETE');fileToken=undefined
   const result=await pending;assert.equal(timedOut,false,'revocation did not end the live upload');assert.ok(result.error||result.status>=400)
   assert.equal((await get(environment,asset,'/home/netlab/files/renamed.txt')).toString(),'A')
   assert.ok(!(await api(assetPath(environment,asset)+'/files?path=/home/netlab/files')).some(item=>item.name.startsWith('.netlab-upload-')),'interrupted upload left a guest staging file')
  }finally{clearTimeout(timer);abort.abort()}
 })
 await step('文件工作区和真实 SSH 终端浏览器操作',async()=>{
  const environment=environments[0]
  const browser=await chromium.launch({channel:'msedge',headless:true})
  try{
   const context=await browser.newContext({viewport:{width:1366,height:900}}),separator=cookie.indexOf('=')
   await context.addCookies([{name:cookie.slice(0,separator),value:cookie.slice(separator+1),url:base}])
   const page=await context.newPage(),errors=[];page.setDefaultTimeout(5000);page.on('pageerror',error=>errors.push(error.message))
   try{
   await page.goto(base+'/environments/'+environment.id)
   await page.getByRole('button',{name:'资产视图',exact:true}).click();await page.getByRole('button',{name:environment.docker.name,exact:true}).click()
   await page.getByRole('button',{name:'对象操作',exact:true}).click();await page.getByRole('menuitem',{name:'文件',exact:true}).click()
   await page.getByRole('table',{name:'文件列表'}).waitFor();await page.getByRole('cell',{name:'data',exact:true}).dblclick();await page.getByRole('cell',{name:'renamed.txt',exact:true}).waitFor()
   await page.getByRole('button',{name:'新建目录',exact:true}).click();await page.getByRole('dialog').getByRole('textbox').fill('browser-dir');await page.getByRole('button',{name:'确定',exact:true}).click();await page.getByRole('cell',{name:'browser-dir',exact:true}).waitFor()
   await page.getByRole('cell',{name:'browser-dir',exact:true}).click({button:'right'});await page.getByRole('menuitem',{name:'重命名',exact:true}).click();await page.getByRole('dialog').getByRole('textbox').fill('browser-renamed');await page.getByRole('button',{name:'确定',exact:true}).click();await page.getByRole('cell',{name:'browser-renamed',exact:true}).waitFor()
   await page.getByRole('dialog').waitFor({state:'hidden'})
   for(const width of [390,1366,1920,2560]){await page.setViewportSize({width,height:width===390?844:900});const size=await page.evaluate(()=>({viewport:innerWidth,content:document.documentElement.scrollWidth}));assert.ok(size.content<=size.viewport,width+' width overflow');await page.getByRole('table',{name:'文件列表'}).waitFor({state:'visible'});if(width===1366||width===390)await page.screenshot({path:'data/files-'+width+'.png'})}
   await page.setViewportSize({width:1366,height:900});await page.getByRole('button',{name:'关闭文件',exact:true}).click();await page.getByRole('button',{name:environment.vm.name,exact:true}).click()
   let output='';page.on('websocket',socket=>{if(socket.url().includes('kind=ssh'))socket.on('framereceived',frame=>{output+=frame.payload.toString()})})
   await page.getByRole('button',{name:'对象操作',exact:true}).click();await page.getByRole('menuitem',{name:'SSH',exact:true}).click();await page.getByRole('tabpanel').getByRole('status').filter({hasText:'已连接'}).waitFor()
   await page.locator('.xterm-helper-textarea').focus();await page.keyboard.type("printf '%s%s\\n' 'SSH_' 'EXECUTED'; stty size");await page.keyboard.press('Enter')
   const deadline=Date.now()+10_000;while(!output.includes('SSH_EXECUTED')&&Date.now()<deadline)await delay(100);assert.ok(output.includes('SSH_EXECUTED'),'SSH command did not execute')
   assert.deepEqual(errors,[])
   await page.getByRole('button',{name:'结束 '+environment.vm.name+' 连接',exact:true}).click()
   }catch(error){await page.screenshot({path:'data/files-error.png'});report.browserText=(await page.locator('body').innerText()).slice(-5000);throw error}
  }finally{await browser.close()}
 })
 await step('清理文件和个人连接设置',async()=>{
  for(const environment of environments){await command(environment,environment.vm,'/home/netlab/files','remove');await api(assetPath(environment,environment.vm)+'/ssh','DELETE');assert.equal((await raw(assetPath(environment,environment.vm)+'/ssh')).status,404)}
  if(fileToken){await api('/service-tokens/'+fileToken.principal.id,'DELETE');fileToken=undefined}
 })
 report.passed=true
}catch(error){report.passed=false;report.error=error.message;process.exitCode=1}
finally{
 for(const environment of environments){try{await completed((await api('/environments/'+environment.id+'/actions','POST',{action:'destroy',clientRequestId:randomUUID()})).id);const state=await api('/environments/'+environment.id+'/state');assert.equal(state.status,'destroyed');assert.equal(state.assets.length,0)}catch(error){report.cleanupErrors.push(error.message);process.exitCode=1}}
 if(fileToken){try{await api('/service-tokens/'+fileToken.principal.id,'DELETE')}catch(error){report.cleanupErrors.push(error.message);process.exitCode=1}}
 if(createdTemplate&&template){try{await completed((await api('/templates/'+template.id,'DELETE')).id)}catch(error){report.cleanupErrors.push(error.message);process.exitCode=1}}
 key?.remove()
 if(report.cleanupErrors.length)report.passed=false
 report.finishedAt=new Date().toISOString();await writeFile('data/files-result.json',JSON.stringify(report,null,2));console.log(JSON.stringify({passed:report.passed,steps:report.steps.length,error:report.error,cleanupErrors:report.cleanupErrors,report:'data/files-result.json'}))
}
