let ws;
const logBox = document.getElementById("log");
const clientsUL = document.getElementById("clients");
const targetsSpan = document.getElementById("targets");
const historyPanel = document.getElementById("historyPanel");
const historyList = document.getElementById("historyList");

const selected = new Set();
const online = new Set();
const rows = new Map();
const lastSeen = new Map();

function log(msg, cls="log-info"){
  const d=document.createElement("div");
  d.className=cls;
  d.textContent=msg;
  logBox.appendChild(d);
  logBox.scrollTop=logBox.scrollHeight;
}

document.getElementById("clearLogBtn").onclick=()=>logBox.innerHTML="";
document.getElementById("runBtn").onclick=execCmd;
document.getElementById("historyBtn").onclick=loadHistory;

document.getElementById("loginBtn").onclick=()=>{
  fetch("/api/login",{method:"POST",headers:{"Content-Type":"application/json"},
    body:JSON.stringify({user:user.value,pass:pass.value})
  }).then(r=>{
    if(r.ok){
      login.style.display="none";
      app.style.display="block";
      connectWS();
    }
  });
};

function connectWS(){
  ws=new WebSocket((location.protocol==="https:"?"wss":"ws")+"://"+location.host+"/ws");
  ws.onmessage=e=>{
    const m=JSON.parse(e.data);
    if(m.type==="init_clients")m.data.forEach(setOnline);
    if(m.type==="client_online")setOnline(m.data);
    if(m.type==="client_offline")setOffline(m.data);
    if(m.type==="task_result"){
      lastSeen.set(m.data.device_id,Date.now());
      log("["+m.data.device_id+"] "+(m.data.output||m.data.error));
    }
  };
}

function ensureRow(id){
  if(rows.has(id))return rows.get(id);
  const li=document.createElement("li");
  li.className="client offline";
  li.innerHTML=`<span>${id}</span>`;
  clientsUL.appendChild(li);
  rows.set(id,li);
  return li;
}
function setOnline(id){
  const r=ensureRow(id);
  r.classList.add("online");
  online.add(id);
  lastSeen.set(id,Date.now());
}
function setOffline(id){
  const r=ensureRow(id);
  r.classList.remove("online");
  online.delete(id);
}

function execCmd(){
  const c=cmd.value.trim();
  if(!c)return;
  fetch("/api/exec",{method:"POST",headers:{"Content-Type":"application/json"},
    body:JSON.stringify({command:c,target:selected.size?[...selected]:"ALL"})
  });
  log("> "+c);
  cmd.value="";
}

async function loadHistory(){
  historyPanel.hidden=false;
  historyList.innerHTML="";
  const r=await fetch("/api/history");
  const arr=await r.json();
  arr.reverse().forEach(h=>{
    const d=document.createElement("div");
    d.textContent=`${h.ts}  ${h.command}`;
    historyList.appendChild(d);
  });
}
