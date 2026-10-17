import { Box } from "lucide-react";
import { CardCarousel } from "../../kit/CardCarousel";
import { modelPurposeLabel, type MachineModel } from "./machines/models";

export function EquipmentModelCarousel({ models, selectedId, onSelect }: {
  models: MachineModel[];
  selectedId?: string;
  onSelect: (id: string) => void;
}) {
  return <CardCarousel items={models} itemKey={model => model.modelId} label="Model selection" noun="model"
    renderItem={model => <button className="fleet-equipment-slot" type="button"
      aria-pressed={selectedId === model.modelId} onClick={() => onSelect(model.modelId)}>
      <Box size={17} aria-hidden />
      <span><strong>{model.modelId}</strong>
        <small>{modelPurposeLabel(model)}</small>
      </span>
    </button>} />;
}
