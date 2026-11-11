import Aura from "@primeuix/themes/aura";
import { definePreset } from "@primeuix/themes";

const STEPS = [50, 100, 200, 300, 400, 500, 600, 700, 800, 900, 950] as const;

function palette(name: string): Record<number, string> {
    return Object.fromEntries(STEPS.map((step) => [step, `{${name}.${step}}`]));
}

export const GostPreset = definePreset(Aura, {
    primitive: {
        borderRadius: {
            none: "0",
            xs: "4px",
            sm: "6px",
            md: "8px",
            lg: "12px",
            xl: "16px",
        },
    },
    semantic: {
        primary: palette("violet"),
        focusRing: { width: "2px", style: "solid", offset: "2px" },
        colorScheme: {
            light: {
                surface: { 0: "#ffffff", ...palette("zinc") },
            },
            dark: {
                surface: { 0: "#ffffff", ...palette("zinc") },
            },
        },
    },
});
