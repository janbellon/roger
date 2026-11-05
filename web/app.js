'use strict'

const HISTORY_KEY = 'roger.history'
const HISTORY_LIMIT = 50
const HISTORY_SHOWN = 5

const HINTS = {
  input: [['↵', 'fetch']],
  probing: [['esc', 'cancel']],
  picking: [['↑↓', 'choose'], ['↵', 'download'], ['esc', 'back']],
  downloading: [['esc', 'cancel']],
  done: [['↵', 'get another']],
  error: [['↵', 'try again']],
}

const $ = id => document.getElementById(id)

const state = {
  view: 'input',
  url: '',
  video: null,
  choice: null,
  jobId: null,
  events: null,
  abort: null,
  fileTTL: 0,
}

function show(view) {
  state.view = view
  for (const name of Object.keys(HINTS)) $(`view-${name}`).hidden = name !== view
  $('hero').hidden = view !== 'input' && view !== 'probing'
  const hints = $('hints')
  hints.replaceChildren(...HINTS[view].map(([key, label]) => {
    const hint = document.createElement('span')
    const kbd = document.createElement('kbd')
    kbd.textContent = key
    hint.append(kbd, label)
    return hint
  }))
}

// formatting, same rules as the terminal version

function formatBytes(bytes) {
  if (!Number.isFinite(bytes) || bytes <= 0) return ''
  const units = ['B', 'KB', 'MB', 'GB']
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit++
  }
  return `${value >= 10 || unit === 0 ? Math.round(value) : value.toFixed(1)} ${units[unit]}`
}

function formatDuration(seconds) {
  if (!Number.isFinite(seconds) || seconds <= 0) return ''
  const s = Math.round(seconds)
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = String(s % 60).padStart(2, '0')
  return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${sec}` : `${m}:${sec}`
}

function isProbablyUrl(input) {
  try {
    const { protocol } = new URL(input.trim())
    return protocol === 'http:' || protocol === 'https:'
  } catch {
    return false
  }
}

// history stays in this browser: the server keeps no record of what you fetched

function loadHistory() {
  try {
    const parsed = JSON.parse(localStorage.getItem(HISTORY_KEY))
    return Array.isArray(parsed) ? parsed.filter(entry => typeof entry?.url === 'string') : []
  } catch {
    return []
  }
}

function saveHistory(entries) {
  try {
    localStorage.setItem(HISTORY_KEY, JSON.stringify(entries))
  } catch {
    // history is a nicety: never let it break a download
  }
}

function addToHistory(url, title) {
  saveHistory([{ url, title }, ...loadHistory().filter(entry => entry.url !== url)].slice(0, HISTORY_LIMIT))
}

function renderHistory() {
  const entries = loadHistory().slice(0, HISTORY_SHOWN)
  $('recent').hidden = entries.length === 0
  $('recent-list').replaceChildren(...entries.map(({ url, title }) => {
    const item = document.createElement('li')
    const button = document.createElement('button')
    button.type = 'button'
    button.className = 'recent-item'
    const name = document.createElement('span')
    name.className = 'recent-title'
    name.textContent = title || url
    const host = document.createElement('span')
    host.className = 'recent-host'
    try {
      host.textContent = new URL(url).hostname.replace(/^www\./, '')
    } catch {
      host.textContent = ''
    }
    button.append(name, host)
    button.addEventListener('click', () => startProbe(url))
    item.append(button)
    return item
  }))
}

// api

async function api(path, options) {
  const response = await fetch(path, options)
  if (response.ok) return response.status === 204 ? null : response.json()
  const body = await response.json().catch(() => null)
  throw new Error(body?.error || `Something went wrong (${response.status}).`)
}

const postJSON = (path, body, signal) => api(path, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify(body),
  signal,
})

// flow: input → probing → picking → downloading → done

function warn(message) {
  const warning = $('url-warning')
  warning.textContent = message
  warning.hidden = !message
}

function backToInput(value = '') {
  state.abort?.abort()
  state.events?.close()
  Object.assign(state, { video: null, choice: null, jobId: null, events: null, abort: null })
  show('input')
  warn('')
  renderHistory()
  const input = $('url')
  input.value = value
  input.focus()
}

function fail(message) {
  state.events?.close()
  state.events = null
  $('error-message').textContent = message
  show('error')
}

async function startProbe(rawUrl) {
  const url = rawUrl.trim()
  if (!isProbablyUrl(url)) {
    warn("That doesn't look like a link. Paste a full URL.")
    return
  }
  warn('')
  state.url = url
  $('probing-url').textContent = url
  show('probing')

  const controller = new AbortController()
  state.abort = controller
  try {
    const video = await postJSON('/api/probe', { url }, controller.signal)
    state.video = video
    renderPicker(video)
    show('picking')
    document.querySelector('.choice')?.focus()
  } catch (error) {
    if (!controller.signal.aborted) fail(error.message)
  }
}

function renderPicker(video) {
  $('video-title').textContent = video.title
  $('video-meta').textContent = [video.platform, formatDuration(video.duration), video.uploader].filter(Boolean).join(' · ')

  const thumb = $('thumb')
  $('thumb-frame').hidden = !video.thumbnail
  thumb.hidden = !video.thumbnail
  if (video.thumbnail) thumb.src = video.thumbnail

  $('choices').replaceChildren(...video.choices.map((choice, index) => {
    const item = document.createElement('li')
    const button = document.createElement('button')
    button.type = 'button'
    button.className = choice.kind === 'audio' ? 'choice choice-audio' : 'choice'
    const label = document.createElement('span')
    label.className = 'choice-label'
    label.textContent = choice.label
    const ext = document.createElement('span')
    ext.className = 'choice-ext'
    ext.textContent = choice.ext ?? ''
    ext.hidden = !choice.ext
    const size = document.createElement('span')
    size.className = 'choice-size'
    size.textContent = choice.size ? `~${formatBytes(choice.size)}` : ''
    button.append(label, ext, size)
    button.addEventListener('click', () => startDownload(index))
    item.append(button)
    return item
  }))
}

async function startDownload(index) {
  const choice = state.video.choices[index]
  state.choice = choice
  $('dl-title').textContent = state.video.title
  $('dl-choice').textContent = [choice.label, choice.ext].filter(Boolean).join(' · ')
  renderProgress({ state: 'queued', parts: 1 })
  show('downloading')

  const controller = new AbortController()
  state.abort = controller
  try {
    const job = await postJSON('/api/downloads', { probe_id: state.video.id, choice: index }, controller.signal)
    state.jobId = job.id
    follow(job.id)
  } catch (error) {
    if (!controller.signal.aborted) fail(error.message)
  }
}

function follow(id) {
  const events = new EventSource(`/api/downloads/${id}/events`)
  state.events = events
  events.onmessage = message => {
    if (state.jobId !== id) return
    const job = JSON.parse(message.data)
    if (job.state === 'ready') {
      events.close()
      finish(job)
    } else if (job.state === 'failed') {
      fail(job.error)
    } else if (job.state === 'cancelled') {
      fail('This download was stopped on the server.')
    } else {
      renderProgress(job)
    }
  }
  // the browser reconnects by itself unless the server answered with an error
  events.onerror = () => {
    if (events.readyState === EventSource.CLOSED && state.jobId === id && state.view === 'downloading') {
      fail('Lost track of this download. Try again.')
    }
  }
}

function setBar(percent) {
  const bar = $('bar')
  bar.classList.toggle('is-indeterminate', percent === null)
  $('bar-fill').style.width = percent === null ? '' : `${percent}%`
  if (percent === null) bar.removeAttribute('aria-valuenow')
  else bar.setAttribute('aria-valuenow', Math.round(percent))
}

function renderProgress(job) {
  const part = job.parts > 1 ? `Part ${job.part + 1} of ${job.parts}` : ''
  const speed = job.speed ? `${formatBytes(job.speed)}/s` : ''
  const eta = job.eta ? `${formatDuration(job.eta)} left` : ''

  if (job.state === 'queued') {
    setBar(null)
    $('dl-status').textContent = 'Waiting for a free slot…'
    $('dl-stats').textContent = ''
  } else if (job.state === 'processing') {
    setBar(null)
    $('dl-status').textContent = state.choice.kind === 'audio' ? 'Converting to mp3…' : 'Merging video and audio…'
    $('dl-stats').textContent = ''
  } else if (job.total > 0) {
    const percent = Math.min(100, (job.downloaded / job.total) * 100)
    setBar(percent)
    $('dl-status').textContent = `${Math.floor(percent)}%`
    $('dl-stats').textContent = [part, speed, eta].filter(Boolean).join(' · ')
  } else if (job.downloaded > 0) {
    setBar(null)
    $('dl-status').textContent = 'Downloading…'
    $('dl-stats').textContent = [part, formatBytes(job.downloaded), speed].filter(Boolean).join(' · ')
  } else {
    setBar(null)
    $('dl-status').textContent = job.refreshing ? 'Link expired, grabbing a fresh one…' : 'Starting download…'
    $('dl-stats').textContent = ''
  }
}

function saveFile(href) {
  const link = document.createElement('a')
  link.href = href
  link.download = ''
  document.body.append(link)
  link.click()
  link.remove()
}

function finish(job) {
  const href = `/api/downloads/${job.id}/file`
  addToHistory(state.url, state.video.title)
  saveFile(href)

  $('done-file').textContent = `${job.filename} · ${formatBytes(job.size)}`
  $('save-again').href = href
  const minutes = Math.round(state.fileTTL / 60)
  $('done-note').textContent = minutes ? `Deleted from the server in ${minutes} min.` : ''
  state.jobId = null
  show('done')
  document.querySelector('#view-done .primary').focus()
}

function cancel() {
  const { jobId, view, url } = state
  if (jobId) api(`/api/downloads/${jobId}`, { method: 'DELETE' }).catch(() => {})
  // keep the link around so a cancel isn't destructive
  backToInput(view === 'probing' || view === 'downloading' ? url : '')
}

// input

$('url-form').addEventListener('submit', event => {
  event.preventDefault()
  startProbe($('url').value)
})

$('url').addEventListener('input', () => warn(''))

// a pasted link needs no extra click
$('url').addEventListener('paste', event => {
  const pasted = event.clipboardData?.getData('text') ?? ''
  if ($('url').value === '' && isProbablyUrl(pasted)) {
    event.preventDefault()
    $('url').value = pasted.trim()
    startProbe(pasted)
  }
})

if (navigator.clipboard?.readText) {
  const paste = $('paste')
  paste.hidden = false
  paste.addEventListener('click', async () => {
    try {
      const text = await navigator.clipboard.readText()
      $('url').value = text.trim()
      if (isProbablyUrl(text)) startProbe(text)
      else $('url').focus()
    } catch {
      $('url').focus() // permission refused: fall back to ctrl+v
    }
  })
}

$('clear-recent').addEventListener('click', () => {
  saveHistory([])
  renderHistory()
})

$('home').addEventListener('click', event => {
  event.preventDefault()
  cancel()
})

document.addEventListener('click', event => {
  const action = event.target.closest('[data-action]')?.dataset.action
  if (action === 'cancel') cancel()
  if (action === 'reset') backToInput()
})

function moveFocus(step, key) {
  const choices = [...document.querySelectorAll('.choice')]
  const current = choices.indexOf(document.activeElement)
  const next = key !== undefined ? key : (current + step + choices.length) % choices.length
  choices[next]?.focus()
}

document.addEventListener('keydown', event => {
  const { view } = state
  if (event.key === 'Escape') {
    if (view === 'probing' || view === 'downloading') cancel()
    else if (view !== 'input') backToInput()
  } else if ((view === 'error' || view === 'done') && event.key === 'Enter') {
    // saving a file can pull focus off the button, so don't rely on it
    if (event.target.closest('a')) return
    event.preventDefault()
    backToInput()
  } else if (view === 'picking') {
    if (event.key === 'ArrowDown' || event.key === 'j') moveFocus(1)
    else if (event.key === 'ArrowUp' || event.key === 'k') moveFocus(-1)
    else if (/^[1-9]$/.test(event.key)) moveFocus(0, Number(event.key) - 1)
    else return
    event.preventDefault()
  }
})

$('thumb').addEventListener('error', () => {
  $('thumb-frame').hidden = true // many CDNs refuse hotlinking
})

// startup

show('input')
renderHistory()
api('/api/meta').then(meta => {
  state.fileTTL = meta.file_ttl_seconds
  $('versions').textContent = `Roger ${meta.version} · yt-dlp ${meta.ytdlp_version}`
}).catch(() => {})
