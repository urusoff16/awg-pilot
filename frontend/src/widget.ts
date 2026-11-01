import './widget.css';
import * as W from '../wailsjs/go/main/Widget';
import {t, tf, setLang, units, perSec} from './i18n';

// History length of the graph: one sample per second, as in DU Meter's mini window.
const SAMPLES = 90;

const rate = (v: number) => {
    const u = units();
    let i = 0;
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    return `${v < 10 && i > 0 ? v.toFixed(1) : Math.round(v)} ${u[i]}${perSec()}`;
};

// runWidget replaces the page with the compact always-on-top status window: a scrolling
// traffic graph (download as green waves, upload as a blue line) with the rates on top.
export function runWidget() {
    document.body.className = 'widget';
    document.documentElement.style.background = 'transparent'; // index.html paints the page dark before scripts run
    document.body.innerHTML = `
        <div class="w-box off" id="wBox" title="Двойной щелчок — открыть AWG Pilot">
            <canvas id="wGraph"></canvas>
            <div class="w-top">
                <span class="w-dot"></span>
                <span class="w-rx" id="wRx">↓ —</span>
                <span class="w-tx" id="wTx">↑ —</span>
                <span class="w-grow"></span>
                <span class="w-ping" id="wPing"></span>
                <button class="w-close" id="wClose">✕</button>
            </div>
            <div class="w-bottom"><span id="wTitle">AWG Pilot</span><span class="w-grow"></span><span id="wPeak"></span></div>
        </div>`;
    const box = document.getElementById('wBox')!;
    const canvas = document.getElementById('wGraph') as HTMLCanvasElement;
    const ctx = canvas.getContext('2d')!;
    document.getElementById('wClose')!.onclick = () => W.Close();
    box.ondblclick = () => W.OpenMain();

    const rx: number[] = [], tx: number[] = [];
    let last: any = null;
    let hover = false; // the grid and the label strip only show on the dark (hovered) panel

    const draw = () => {
        const dpr = window.devicePixelRatio || 1;
        const w = box.clientWidth, h = box.clientHeight;
        if (canvas.width !== Math.round(w * dpr) || canvas.height !== Math.round(h * dpr)) {
            canvas.width = Math.round(w * dpr);
            canvas.height = Math.round(h * dpr);
        }
        ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
        ctx.clearRect(0, 0, w, h);

        if (hover) { // grid: DU Meter's dark green lattice
            ctx.strokeStyle = 'rgba(70, 140, 100, .16)';
            ctx.lineWidth = 1;
            ctx.beginPath();
            for (let y = h; y > 0; y -= h / 4) { ctx.moveTo(0, Math.round(y) + .5); ctx.lineTo(w, Math.round(y) + .5); }
            for (let x = w; x > 0; x -= w / 9) { ctx.moveTo(Math.round(x) + .5, 0); ctx.lineTo(Math.round(x) + .5, h); }
            ctx.stroke();
        }

        const peak = Math.max(1024, ...rx, ...tx) * 1.15;
        const step = w / (SAMPLES - 1);
        const xAt = (i: number, n: number) => w - (n - 1 - i) * step;
        const base = h - 15; // the bottom strip holds the labels, the graph stays above it
        const yAt = (v: number) => base - (base - 24) * v / peak;

        if (rx.length > 1) {
            const n = rx.length;
            const fill = ctx.createLinearGradient(0, 22, 0, base);
            fill.addColorStop(0, hover ? 'rgba(70, 235, 140, .85)' : 'rgba(70, 235, 140, .45)');
            fill.addColorStop(1, hover ? 'rgba(20, 120, 70, .25)' : 'rgba(20, 120, 70, .08)');
            ctx.beginPath();
            ctx.moveTo(xAt(0, n), base);
            rx.forEach((v, i) => ctx.lineTo(xAt(i, n), yAt(v)));
            ctx.lineTo(w, base);
            ctx.closePath();
            ctx.fillStyle = fill;
            ctx.fill();
            ctx.beginPath();
            rx.forEach((v, i) => i ? ctx.lineTo(xAt(i, n), yAt(v)) : ctx.moveTo(xAt(i, n), yAt(v)));
            ctx.strokeStyle = '#3fe08e'; // solid lines: they must stay visible on any wallpaper
            ctx.lineWidth = 1.8;
            ctx.stroke();

            ctx.beginPath();
            tx.forEach((v, i) => i ? ctx.lineTo(xAt(i, n), yAt(v)) : ctx.moveTo(xAt(i, n), yAt(v)));
            ctx.strokeStyle = '#4f9dff';
            ctx.lineWidth = 1.8;
            ctx.stroke();
        }
        if (hover) {
            ctx.fillStyle = 'rgba(0, 0, 0, .6)';
            ctx.fillRect(0, base, w, h - base);
            ctx.fillStyle = 'rgba(70, 140, 100, .35)';
            ctx.fillRect(0, base, w, 1);
        }
        document.getElementById('wPeak')!.textContent = rx.length > 1 ? tf('макс {0}', rate(peak / 1.15)) : '';
    };

    const update = async () => {
        let s: any;
        try {
            s = await W.Status();
        } catch {
            s = {state: 'bad', title: t('Нет связи со службой'), detail: '', rx: 0, tx: 0, ping: -1};
        }
        if (s.lang) setLang(s.lang);
        last = s;
        box.className = `w-box ${s.state}`;
        const up = s.state === 'ok';
        rx.push(up ? s.rx : 0); tx.push(up ? s.tx : 0);
        if (rx.length > SAMPLES) { rx.shift(); tx.shift(); }
        document.getElementById('wRx')!.textContent = up ? `↓ ${rate(s.rx)}` : s.title;
        document.getElementById('wTx')!.textContent = up ? `↑ ${rate(s.tx)}` : '';
        document.getElementById('wPing')!.textContent = up && s.ping >= 0 ? `${s.ping} мс` : '';
        document.getElementById('wTitle')!.textContent = up ? 'AWG Pilot' : s.detail;
        box.title = `${s.title}${s.detail ? ' — ' + s.detail : ''}\n${t('Двойной щелчок — открыть AWG Pilot')}`;
        document.getElementById('wClose')!.title = t('Скрыть окошко');
        draw();
    };
    window.addEventListener('resize', () => last && draw());
    box.addEventListener('mouseenter', () => { hover = true; draw(); });
    box.addEventListener('mouseleave', () => { hover = false; draw(); });
    update();
    window.setInterval(update, 1000);
}
