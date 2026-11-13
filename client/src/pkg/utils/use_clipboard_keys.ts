import { onBeforeUnmount, onMounted } from "vue";

export interface ClipboardKeys {
    cut: () => void;
    paste: () => void;
}

// Runs cut on ctrl+x and paste on ctrl+v (cmd on macOS) while the component is mounted. In a text field, or while
// a modal dialog is open, the keys keep their normal meaning. A held key runs once.
export function useClipboardKeys(handlers: ClipboardKeys): void {
    function onKeydown(event: KeyboardEvent): void {
        if (!(event.ctrlKey || event.metaKey) || event.altKey || event.shiftKey || event.repeat) return;
        if (isTextField(event.target) || document.querySelector("[aria-modal='true']")) return;

        const key = event.key.toLowerCase();
        if (key === "x") {
            event.preventDefault();
            handlers.cut();
        } else if (key === "v") {
            event.preventDefault();
            handlers.paste();
        }
    }

    onMounted(() => window.addEventListener("keydown", onKeydown));
    onBeforeUnmount(() => window.removeEventListener("keydown", onKeydown));
}

// A row checkbox is an input too, and the keys must work after a click on it.
const NON_TEXT_INPUTS = new Set(["checkbox", "radio", "button", "submit", "reset", "file", "image", "range", "color"]);

function isTextField(target: EventTarget | null): boolean {
    if (target instanceof HTMLInputElement) return !NON_TEXT_INPUTS.has(target.type);
    return (
        target instanceof HTMLTextAreaElement ||
        target instanceof HTMLSelectElement ||
        (target instanceof HTMLElement && target.isContentEditable)
    );
}
