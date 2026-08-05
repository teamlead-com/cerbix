const i=[{key:"summary",heading:"Summary",placeholder:"What happened, in a sentence or two."},{key:"rootCause",heading:"Root cause",placeholder:"Why it happened."},{key:"resolution",heading:"Resolution",placeholder:"How it was fixed."},{key:"actionItems",heading:"Action items",placeholder:"- Prevent recurrence…"}];function l(){return{summary:"",rootCause:"",resolution:"",actionItems:""}}function h(t){return i.filter(e=>t[e.key].trim()).map(e=>`## ${e.heading}

${t[e.key].trim()}`).join(`

`)}function d(t){const e=l();if(!t)return e;const n=t.split(/^##\s+/m);let a=!1;for(const o of n){const r=o.indexOf(`
`),c=(r===-1?o:o.slice(0,r)).trim().toLowerCase(),m=(r===-1?"":o.slice(r+1)).trim(),s=i.find(u=>u.heading.toLowerCase()===c);s&&(e[s.key]=m,a=!0)}return a||(e.summary=t.trim()),e}function p(t){const e=d(t);return i.filter(n=>e[n.key].trim()).map(n=>({heading:n.heading,content:e[n.key].trim()}))}export{i as P,l as e,d as p,p as r,h as s};
