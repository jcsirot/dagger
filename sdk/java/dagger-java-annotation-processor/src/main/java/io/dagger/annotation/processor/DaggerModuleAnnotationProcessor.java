package io.dagger.annotation.processor;

import java.io.IOException;
import java.io.PrintWriter;
import java.util.HashSet;
import java.util.Set;

import javax.annotation.processing.AbstractProcessor;
import javax.annotation.processing.Processor;
import javax.annotation.processing.RoundEnvironment;
import javax.annotation.processing.SupportedAnnotationTypes;
import javax.annotation.processing.SupportedSourceVersion;
import javax.lang.model.SourceVersion;
import javax.lang.model.element.Element;
import javax.lang.model.element.ElementKind;
import javax.lang.model.element.ExecutableElement;
import javax.lang.model.element.TypeElement;
import javax.tools.FileObject;
import javax.tools.StandardLocation;

import com.google.auto.service.AutoService;

import jakarta.json.bind.Jsonb;
import jakarta.json.bind.JsonbBuilder;

@SupportedAnnotationTypes({
    "io.dagger.module.annotation.ModuleObject", 
    "io.dagger.module.annotation.ModuleFunction"
})
@SupportedSourceVersion(SourceVersion.RELEASE_17)
@AutoService(Processor.class)
public class DaggerModuleAnnotationProcessor extends AbstractProcessor {

    @Override
    public boolean process(Set<? extends TypeElement> annotations, RoundEnvironment roundEnv) {
        Set<String> annotatedObjects = new HashSet<>();
        Set<String> annotatedFunctions = new HashSet<>();

        System.out.println("Annotation Processor");
        for (TypeElement annotation : annotations) {
            for (Element element : roundEnv.getElementsAnnotatedWith(annotation)) {
                if (element.getKind() == ElementKind.CLASS || element.getKind() == ElementKind.RECORD) {
                    TypeElement typeElement = (TypeElement) element;
                    annotatedObjects.add(typeElement.getQualifiedName().toString());
                } else if (element.getKind() == ElementKind.METHOD) {
                    ExecutableElement executableElement = (ExecutableElement) element;
                    TypeElement enclosingType = (TypeElement) executableElement.getEnclosingElement();
                    annotatedFunctions.add(enclosingType.getQualifiedName().toString() + "#" + executableElement.getSimpleName().toString());
                }
            }
        }

        System.out.println(annotatedObjects);
        System.out.println(annotatedFunctions);
        
        if (!annotatedObjects.isEmpty()) {
            try {
                FileObject resource = processingEnv.getFiler().createResource(
                    StandardLocation.CLASS_OUTPUT, "", "dagger_module_info.json");
                try (PrintWriter out = new PrintWriter(resource.openWriter())) {
                    writeClasses(annotatedObjects, out);
                }
            } catch (IOException ioe) {
                throw new RuntimeException(ioe);
            }
        }
        
        return true;
    }

    private void writeClasses(Set<String> annotatedClasses, PrintWriter out) throws IOException {
        ModuleInfo moduleInfo = new ModuleInfo(annotatedClasses.toArray(new String[annotatedClasses.size()]));
        Jsonb jsonb = JsonbBuilder.create();
        String serialized = jsonb.toJson(moduleInfo);
        out.print(serialized);
    }
}
