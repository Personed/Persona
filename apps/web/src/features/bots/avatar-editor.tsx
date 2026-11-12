import { BotAvatar } from "@persona/ui/avatar";
import {
  characterColors,
  characterIdentity,
  characterNames,
  characterSeed,
} from "@persona/ui/characters";
import { Button } from "@persona/ui/controls";
import { Popover } from "@persona/ui/popover";
import { ArrowsClockwiseIcon } from "@phosphor-icons/react/dist/csr/ArrowsClockwise";
import { PencilSimpleIcon } from "@phosphor-icons/react/dist/csr/PencilSimple";
import { useI18n } from "../i18n/context.tsx";
import "./profile.css";

const shapeLabelsPt: Record<string, string> = {
  soft: "Quadrado suave",
  round: "Círculo",
  drop: "Gota",
  triangle: "Triângulo arredondado",
  heart: "Coração",
  hex: "Hexágono",
  star: "Estrela",
  capsule: "Cápsula",
  cloud: "Nuvem",
};

const colorLabelsPt: Record<string, string> = {
  "#ff776b": "Coral",
  "#fa638a": "Framboesa",
  "#ffa595": "Pêssego",
  "#ffb675": "Damasco",
  "#ff994f": "Laranja",
  "#f8bd3e": "Âmbar",
  "#f4df57": "Limão",
  "#c6e95b": "Lima",
  "#91df74": "Verde claro",
  "#7ce6bc": "Menta",
  "#46d99e": "Esmeralda",
  "#4dd9cc": "Turquesa",
  "#62d5e9": "Ciano",
  "#7bc6ff": "Azul celeste",
  "#659fff": "Azul céu",
  "#a79aff": "Pervinca",
  "#c7a0f6": "Lilás",
  "#f69dd1": "Rosa cerejeira",
};

export function AvatarEditor({
  value,
  onChange,
}: {
  value: string;
  onChange: (value: string) => void;
}) {
  const { t, language } = useI18n();
  const isPt = language === "pt-BR";
  const identity = characterIdentity(value);
  return (
    <div className="avatar-editor">
      <BotAvatar seed={value} size="large" />
      <Popover.Root>
        <Popover.Trigger
          render={
            <Button
              type="button"
              variant="secondary"
              size="icon-small"
              aria-label={t("选择头像")}
              title={t("选择头像")}
            />
          }
        >
          <PencilSimpleIcon size={16} />
        </Popover.Trigger>
        <Popover.Portal>
          <Popover.Positioner
            className="picker-positioner"
            sideOffset={8}
            collisionPadding={12}
            align="start"
          >
            <Popover.Popup className="picker-popup avatar-picker">
              <Popover.Title className="sr-only">{t("头像外观")}</Popover.Title>
              <fieldset className="avatar-shapes" aria-label={t("外形")}>
                {(
                  Object.keys(characterNames) as (keyof typeof characterNames)[]
                ).map((shape) => {
                  const shapeTitle = isPt
                    ? (shapeLabelsPt[shape] ?? characterNames[shape])
                    : characterNames[shape];
                  return (
                    <Button
                      key={shape}
                      variant="ghost"
                      aria-label={
                        isPt ? `Formato: ${shapeTitle}` : `外形：${shapeTitle}`
                      }
                      title={shapeTitle}
                      aria-pressed={identity.shape === shape}
                      onClick={() =>
                        onChange(characterSeed(shape, identity.color))
                      }
                    >
                      <BotAvatar
                        seed={characterSeed(shape, identity.color)}
                        size="medium"
                      />
                    </Button>
                  );
                })}
              </fieldset>
              <fieldset className="avatar-colors" aria-label={t("颜色")}>
                {characterColors.map(({ name, color }) => {
                  const colorTitle = isPt
                    ? (colorLabelsPt[color] ?? name)
                    : name;
                  return (
                    <Button
                      key={color}
                      variant="ghost"
                      aria-label={isPt ? `Cor: ${colorTitle}` : `颜色：${name}`}
                      title={colorTitle}
                      aria-pressed={identity.color === color}
                      onClick={() =>
                        onChange(characterSeed(identity.shape, color))
                      }
                    >
                      <span style={{ backgroundColor: color }} />
                    </Button>
                  );
                })}
              </fieldset>
              <Button
                type="button"
                variant="ghost"
                size="small"
                aria-label={t("换个形象")}
                title={t("换个形象")}
                onClick={() => onChange(characterSeed())}
              >
                <ArrowsClockwiseIcon size={16} />
              </Button>
            </Popover.Popup>
          </Popover.Positioner>
        </Popover.Portal>
      </Popover.Root>
    </div>
  );
}
