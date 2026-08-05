import { ref } from "vue";

type Theme = "light" | "dark";
const STORAGE_KEY = "cerbix-theme";

function current(): Theme {
  const attr = document.documentElement.getAttribute("data-theme");
  if (attr === "dark" || attr === "light") return attr;
  return matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

const theme = ref<Theme>(current());

export function useTheme() {
  function set(next: Theme) {
    theme.value = next;
    document.documentElement.setAttribute("data-theme", next);
    try {
      localStorage.setItem(STORAGE_KEY, next);
    } catch {
      /* ignore */
    }
  }
  function toggle() {
    set(current() === "dark" ? "light" : "dark");
  }
  return { theme, toggle, set };
}
