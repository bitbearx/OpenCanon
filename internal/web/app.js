'use strict';
const $ = (s) => document.querySelector(s);
let documentData = null;
let profile = null;
let copies = 1;
let uploading = false;
let sending = false;
let locked = false;
let objectURL = null;
let pollTimer = null;
let checking = false;
let confirmedJob = null;
let pollDeadlineTimer = null;
let pollController = null;
let pollGeneration = 0;
let autoScanAttempted = false;

// renderDiscovery 显示已保存地址与发现时间，不将缓存当作在线证明。
function renderDiscovery(discovery, scanError = '') {
  $('#printerAddress').textContent = discovery?.address || '尚未发现地址';
  const error = scanError || discovery?.lastError || '';
  if (discovery?.address) {
    const at = new Date(discovery.discoveredAt);
    const stamp = Number.isNaN(at.getTime()) ? '时间未知' : at.toLocaleString('zh-CN', { hour12: false });
    $('#discoveryStatus').textContent = `${discovery.name || '打印机'} · 上次发现 ${stamp}。${error ? '本次扫描失败：' + error + '；保留上次地址。' : '地址已保存，点击重新扫描可更新。'}`;
  } else $('#discoveryStatus').textContent = error || '尚未发现打印机，点击扫描重试。';
}

// parseRange 独立规范化页码，拒绝空片段、倒序与越界。
function parseRange(text, total) {
  if (!Number.isInteger(total) || total < 1) return null;
  const value = text.trim().replace(/，/g, ',');
  if (!value) return { count: total, normalized: '' };
  const spans = [];
  for (const part of value.split(',')) {
    const m = /^\s*(\d+)\s*(?:-\s*(\d+)\s*)?$/.exec(part);
    if (!m) return null;
    const a = Number(m[1]), b = Number(m[2] || m[1]);
    if (!Number.isSafeInteger(a) || !Number.isSafeInteger(b) || a < 1 || a > b || b > total) return null;
    spans.push([a, b]);
  }
  spans.sort((a, b) => a[0] - b[0]);
  const merged = [];
  for (const span of spans) {
    const last = merged[merged.length - 1];
    if (last && span[0] <= last[1] + 1) last[1] = Math.max(last[1], span[1]);
    else merged.push(span.slice());
  }
  return { count: merged.reduce((n, [a, b]) => n + b - a + 1, 0), normalized: merged.map(([a, b]) => a === b ? String(a) : `${a}-${b}`).join(',') };
}

// estimateSheets 按逐份排序计算奇数页双面用纸。
function estimateSheets(pages, count, duplex) {
  return (duplex === 'single' ? pages : Math.ceil(pages / 2)) * count;
}

// api 统一解释服务端错误并保留状态码。
async function api(path, options = {}) {
  const response = await fetch(path, { credentials: 'same-origin', ...options });
  let data;
  try { data = await response.json(); } catch { throw new Error('服务器返回无效响应，请重新检查服务。'); }
  if (!response.ok) {
    const error = new Error(data.error?.message || `请求失败（${response.status}）`);
    error.status = response.status;
    error.code = data.error?.code;
    throw error;
  }
  return data;
}

// setError 使用纯文本展示可操作错误。
function setError(message) {
  $('#fileErr').textContent = message;
  $('#fileErr').hidden = !message;
}

// selectedLabel 返回设备能力选项的展示名。
function selectedLabel(id) {
  return $(id).selectedOptions[0]?.textContent || '暂无可用选项';
}

// legalCombination 与服务端共用禁止组合的三元组。
function legalCombination(mediaId, colorMode, duplex) {
  return !(profile?.constraints || []).some((c) => c.mediaId === mediaId && c.colorMode === colorMode && c.duplex === duplex);
}

// fillOptions 从已验证的能力填充控件，优先指定的业务值。
function fillOptions(id, options, preferred) {
  const select = $(id), previous = select.value, previousLabel = selectedLabel(id);
  select.replaceChildren();
  for (const value of options || []) {
    const option = document.createElement('option');
    option.value = value.id;
    option.textContent = value.label;
    select.append(option);
  }
  const ids = (options || []).map((o) => o.id);
  if (previous && !ids.includes(previous)) {
    const unavailable = document.createElement('option');
    unavailable.value = previous; unavailable.textContent = previousLabel + '（不可用，请重选）';
    unavailable.disabled = true; select.append(unavailable); select.value = previous;
  } else select.value = ids.includes(previous) ? previous : ids.includes(preferred) ? preferred : ids[0] || '';
}

// checkPrinter 加载保存的地址；显式点击时重新执行后端发现。
async function checkPrinter(scan = false) {
  if (checking || sending || $('#dlg').open) return;
  const initial = !$('#size').value && !$('#color').value && !$('#duplex').value;
  checking = true;
  $('#recheckBtn').disabled = true;
  $('#recheckLabel').textContent = scan ? '扫描中…' : '检查中…';
  let scanError = '';
  update();
  try {
    if (scan) {
      autoScanAttempted = true;
      $('#discoveryStatus').textContent = '正在扫描局域网中的打印机…';
      try {
        await api('/api/printer/scan', { method: 'POST', headers: { 'X-OpenCanon-Request': '1' } });
      } catch (error) { if (error.code !== 'scan_in_progress') scanError = error.message; }
    }
    profile = await api('/api/printer');
    const scanDeadline = Date.now() + 25000;
    while (profile.discovery?.scanning) {
      autoScanAttempted = true;
      if (Date.now() >= scanDeadline) throw new Error('扫描仍在进行，请稍后重新扫描。');
      $('#discoveryStatus').textContent = '其他页面正在扫描打印机，正在等待发现结果…';
      await new Promise((resolve) => setTimeout(resolve, 1000));
      profile = await api('/api/printer');
    }
    document.body.classList.toggle('queue-available', !!profile.available);
    $('#printerState').textContent = profile.available ? '佳能 · 设备就绪' : '佳能 · 暂不可打印';
    $('#printerReason').textContent = profile.stateReason || (profile.available ? '已直接连接打印机；打印结果以实际出纸为准。' : '请检查打印机配置。');
    renderDiscovery(profile.discovery, scanError);
    fillOptions('#size', profile.media, 'A4');
    fillOptions('#color', profile.colorModes, 'mono');
    fillOptions('#duplex', profile.duplexModes, 'single');
    if (initial && !legalCombination($('#size').value, $('#color').value, $('#duplex').value)) {
      let found = false;
      // 兜底组合也优先单色，仅在单色不可用时退回彩色。
      const colors = [...(profile.colorModes || [])].sort((a, b) => (b.id === 'mono') - (a.id === 'mono'));
      for (const m of profile.media || []) {
        for (const c of colors) {
          for (const d of profile.duplexModes || []) {
            if (!found && legalCombination(m.id, c.id, d.id)) {
              $('#size').value = m.id; $('#color').value = c.id; $('#duplex').value = d.id; found = true;
            }
          }
        }
      }
    }
  } catch (error) {
    profile = null;
    document.body.classList.remove('queue-available');
    $('#printerState').textContent = '无法连接服务';
    $('#printerReason').textContent = error.message;
  } finally {
    checking = false;
    $('#recheckBtn').disabled = false;
    $('#recheckLabel').textContent = profile?.discovery?.address ? '重新扫描' : '扫描打印机';
    update();
  }
  if (!scan && profile && !autoScanAttempted && !profile.discovery?.address) {
    autoScanAttempted = true;
    await checkPrinter(true);
  }
}

// currentRange 空字符串表示全部，指定范围必须有内容。
function currentRange() {
  if (!documentData) return null;
  if ($('#rangeMode').value === 'custom' && !$('#rangeText').value.trim()) return null;
  return parseRange($('#rangeMode').value === 'custom' ? $('#rangeText').value : '', documentData.pageCount);
}

// update 同步页码反馈、用纸估算和可提交状态。
function update() {
  const has = !!documentData;
  $('#empty').hidden = has;
  $('#filled').hidden = !has;
  $('#panel').hidden = !has;
  $('#dock').hidden = !has;
  $('#layout').classList.toggle('split', has);
  document.body.classList.toggle('has-dock', has);
  $('#file').disabled = uploading || sending;
  $('#replaceBtn').disabled = uploading || sending;
  $('#removeBtn').disabled = uploading || sending;
  $('#copies').value = copies;
  $('#minus').disabled = copies <= 1 || sending || locked;
  $('#plus').disabled = copies >= 99 || sending || locked;
  for (const id of ['#size', '#color', '#duplex', '#rangeMode', '#rangeText']) $(id).disabled = sending || locked || uploading;
  $('#rangeRow').hidden = !has || documentData.kind !== 'pdf' || documentData.pageCount <= 1;
  $('#pageRow').hidden = $('#rangeRow').hidden || $('#rangeMode').value !== 'custom';
  const range = currentRange();
  $('#rangeErr').hidden = !has || !!range;
  $('#rangeErr').textContent = `页码应在 1–${documentData?.pageCount || 1} 内，例如 1-3,5；不接受倒序或空片段。`;
  const duplex = $('#duplex').value;
  $('#colorHint').textContent = $('#color').value === 'mono' ? '将内容以黑白或灰度打印；实际用墨由打印机决定。' : '照片和彩色图表可以选择彩色。';
  $('#duplexHint').textContent = duplex === 'single' ? '每张纸只印一面，背面空着。' : duplex ? '由打印机自动翻纸。竖向长边通常左右翻、短边通常上下翻；横向文档相反。' : '尚未取得驱动能力。';
  const legal = legalCombination($('#size').value, $('#color').value, duplex)
    && (profile?.media || []).some((m) => m.id === $('#size').value)
    && (profile?.colorModes || []).some((c) => c.id === $('#color').value)
    && (profile?.duplexModes || []).some((d) => d.id === duplex);
  $('#ticketMain').textContent = `${selectedLabel('#size')} · ${selectedLabel('#color')} · ${copies} 份`;
  $('#ticketSub').textContent = !legal ? '此纸张、色彩与双面组合不受支持' : range ? `${range.count} 页／份 · 约 ${estimateSheets(range.count, copies, duplex || 'single')} 张纸` : '请检查页码';
  $('#printBtn').disabled = !has || !range || !legal || !profile?.available || checking || uploading || sending || locked;
}

// load 上传单个文档；失败保留旧文档与设置。
async function load(files) {
  if (uploading || sending) return;
  if (!files?.length) return;
  if (files.length !== 1) { setError('一次请选择一个文件。'); return; }
  const file = files[0];
  if (file.size > 20 * 1024 * 1024) { setError('文件超过 20 MiB，请缩小后重新上传。'); return; }
  uploading = true; setError('正在上传并解析文件…'); update();
  try {
    const body = new FormData(); body.append('file', file);
    const data = await api('/api/documents', { method: 'POST', body });
    if (objectURL) URL.revokeObjectURL(objectURL);
    objectURL = data.kind === 'image' ? URL.createObjectURL(file) : null;
    documentData = data; locked = false;
    $('#rangeMode').value = 'all'; $('#rangeText').value = '';
    renderFile(file);
    setError('');
  } catch (error) { setError(error.message); }
  finally { uploading = false; $('#file').value = ''; update(); }
}

// renderFile 只展示真实页数；浏览器无法解码图片时改用服务器预览。
function renderFile(file) {
  $('#fname').textContent = documentData.name;
  $('#fsub').textContent = `${documentData.kind === 'pdf' ? 'PDF' : '图片'} · ${documentData.pageCount} 页 · ${(file.size / 1048576).toFixed(1)} MiB`;
  $('#preview').replaceChildren();
  if (documentData.kind === 'image') {
    const image = new Image();
    image.alt = '待打印图片预览';
    image.onerror = () => {
      image.onerror = () => { setError('图片预览暂不可用，可重新上传或检查服务。'); };
      if (documentData.previewUrl) image.src = documentData.previewUrl;
    };
    image.src = objectURL || documentData.previewUrl;
    $('#preview').append(image);
  } else {
    const card = document.createElement('div'); card.className = 'docph';
    card.textContent = `PDF 文档 · 共 ${documentData.pageCount} 页`;
    $('#preview').append(card);
  }
}

// buildRecap 用纯文本建立确认工单，避免文件名注入。
function buildRecap() {
  const range = currentRange();
  confirmedJob = { documentId: documentData.id, mediaId: $('#size').value, pageRanges: range.normalized, copies, colorMode: $('#color').value, duplex: $('#duplex').value };
  const rows = [['文件', documentData.name], ['范围', range.normalized || `全部 ${documentData.pageCount} 页`], ['尺寸', selectedLabel('#size')], ['份数', `${copies} 份`], ['色彩', selectedLabel('#color')], ['双面', selectedLabel('#duplex')], ['用纸', `约 ${estimateSheets(range.count, copies, $('#duplex').value)} 张`]];
  $('#recap').replaceChildren();
  for (const [key, value] of rows) {
    const dt = document.createElement('dt'), dd = document.createElement('dd');
    dt.textContent = key; dd.textContent = value; $('#recap').append(dt, dd);
  }
  $('#dlgNote').textContent = $('#duplex').value !== 'single' && range.count % 2 ? '每份最后一张纸背面留白。任务将直接发送到设备；实际输出请查看打印机。' : '任务将直接发送到设备；实际输出请查看打印机。';
}

// showView 切换确认、提交和任务反馈。
function showView(id) {
  for (const name of ['viewConfirm', 'viewSending', 'viewDone']) $('#' + name).hidden = name !== id;
}

// stopPolling 取消计时与在途请求，使旧任务不能覆盖新任务。
function stopPolling() {
  clearTimeout(pollTimer); clearTimeout(pollDeadlineTimer);
  pollController?.abort(); pollController = null; pollGeneration++;
}

// pollJob 独立截止计时确保挂起请求也在 60 秒时停止。
function pollJob(id) {
  stopPolling();
  const generation = pollGeneration;
  pollController = new AbortController();
  const signal = pollController.signal;
  pollDeadlineTimer = setTimeout(() => {
    if (generation !== pollGeneration) return;
    stopPolling();
    $('#jobStatus').textContent = `任务 ${id}：已停止自动查询。请检查打印机面板和实际出纸。`;
  }, 60000);
  // tick 仅允许当前代次更新状态与继续安排查询。
  async function tick() {
    try {
      const job = await api('/api/print-jobs/' + encodeURIComponent(id), { signal });
      if (generation !== pollGeneration || !$('#dlg').open) return;
      const labels = { waiting: '等待打印', processing: '正在处理', failed: '打印任务失败', unknown: '无法确认最终状态' };
      $('#jobStatus').textContent = `任务 ${id}：${labels[job.state] || '已发送'}。${job.stateReason || job.message || ''}`;
      if (['unknown', 'failed'].includes(job.state)) { stopPolling(); return; }
    } catch (error) {
      if (generation !== pollGeneration || signal.aborted) return;
      $('#jobStatus').textContent = `任务 ${id}：${error.message} 请检查打印机面板和实际出纸。`;
    }
    if (generation === pollGeneration && $('#dlg').open) pollTimer = setTimeout(tick, 3000);
  }
  pollTimer = setTimeout(tick, 3000);
}

// submitJob 网络结果不明时锁定文档，避免重复纸张。
async function submitJob() {
  if (sending || locked || checking || !confirmedJob || $('#printBtn').disabled) return;
  sending = true; update(); showView('viewSending');
  try {
    const job = await api('/api/print-jobs', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-OpenCanon-Request': '1' }, body: JSON.stringify(confirmedJob) });
    locked = true;
    showView('viewDone');
    $('#jobStatus').textContent = `任务 ${job.id} 的数据已被设备接收。请检查实际出纸。`;
    pollJob(job.id);
  } catch (error) {
    if (!error.status || /uncertain/i.test(error.code || '')) {
      locked = true;
      setError('发送结果不明，请先检查打印机和实际出纸；此文档已禁止再次提交。');
    } else {
      if (error.status === 404) { locked = true; setError('文档已过期或服务已重启，请重新上传。'); }
      else setError(error.message);
    }
    $('#dlg').close();
  } finally { sending = false; update(); }
}

// reset 释放浏览器预览，不影响设备已接收的任务。
function reset() {
  if (sending || uploading) return;
  if (objectURL) URL.revokeObjectURL(objectURL);
  objectURL = null; documentData = null; locked = false; copies = 1;
  $('#rangeMode').value = 'all'; $('#rangeText').value = ''; setError(''); update();
}
$('#file').addEventListener('change', (e) => load(e.target.files));
$('#replaceBtn').addEventListener('click', () => $('#file').click());
$('#removeBtn').addEventListener('click', reset);
$('#recheckBtn').addEventListener('click', () => checkPrinter(true));
for (const id of ['#size', '#color', '#duplex', '#rangeMode']) $(id).addEventListener('change', update);
$('#rangeText').addEventListener('input', update);
$('#minus').addEventListener('click', () => { copies = Math.max(1, copies - 1); update(); });
$('#plus').addEventListener('click', () => { copies = Math.min(99, copies + 1); update(); });
$('#printBtn').addEventListener('click', () => { if (!$('#printBtn').disabled) { buildRecap(); showView('viewConfirm'); $('#dlg').showModal(); document.body.classList.add('modal-open'); } });
$('#confirmBtn').addEventListener('click', submitJob);
$('#cancelBtn').addEventListener('click', () => $('#dlg').close());
$('#doneBtn').addEventListener('click', () => { $('#dlg').close(); reset(); });
$('#dlg').addEventListener('cancel', (e) => { if (sending) e.preventDefault(); });
$('#dlg').addEventListener('close', () => { stopPolling(); confirmedJob = null; document.body.classList.remove('modal-open'); });
for (const event of ['dragenter', 'dragover', 'dragleave', 'drop']) $('#stage').addEventListener(event, (e) => {
  e.preventDefault(); $('#stage').classList.toggle('dragover', event === 'dragenter' || event === 'dragover');
  if (event === 'drop') load(e.dataTransfer.files);
});
update(); checkPrinter();
